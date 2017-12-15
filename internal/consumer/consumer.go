// Copyright (c) 2017 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package consumer

import (
	"fmt"
	"sync"

	cluster "github.com/bsm/sarama-cluster"
	"github.com/uber-go/kafka-client/internal/metrics"
	"github.com/uber-go/kafka-client/internal/util"
	"github.com/uber-go/kafka-client/kafka"
	"github.com/uber-go/tally"
	"go.uber.org/zap"
)

type (
	partitionMap struct {
		partitions map[int32]*partitionConsumer
	}
	// consumerImpl is an implementation of kafka consumer
	consumerImpl struct {
		name       string
		topic      string
		dlqTopic   string
		consumer   SaramaConsumer
		partitions partitionMap
		msgCh      chan kafka.Message
		dlq        DLQ
		tally      tally.Scope
		logger     *zap.Logger
		options    *Options
		lifecycle  *util.RunLifecycle
		stopC      chan struct{}
		doneC      chan struct{}
	}
)

// New returns a new kafka consumer for a given topic
// the returned consumer can be used to consume and process
// messages across multiple go routines. The number of routines
// that will process messages in parallel MUST be pre-configured
// through the ConsumerConfig. And after each message is processed,
// either msg.Ack or msg.Nack must be called to advance the offsets
//
// During failures / partition rebalances, this consumer does a
// best effort at avoiding duplicates, but the application must be
// designed for idempotency
func New(
	config *kafka.ConsumerConfig,
	consumer SaramaConsumer,
	options *Options,
	dlq DLQ,
	scope tally.Scope,
	log *zap.Logger) (kafka.Consumer, error) {
	return newConsumer(config, consumer, options, dlq, scope, log)
}

// newConsumer is package private constructor that actually does the construction
func newConsumer(config *kafka.ConsumerConfig,
	consumer SaramaConsumer,
	options *Options,
	dlq DLQ,
	scope tally.Scope,
	log *zap.Logger) (*consumerImpl, error) {
	return &consumerImpl{
		name:       config.GroupName,
		topic:      config.Topic,
		dlqTopic:   config.DLQ.Name,
		consumer:   consumer,
		dlq:        dlq,
		msgCh:      make(chan kafka.Message, options.RcvBufferSize),
		partitions: newPartitionMap(),
		tally:      scope.Tagged(map[string]string{"topic": config.Topic}),
		logger:     log,
		options:    options,
		stopC:      make(chan struct{}),
		doneC:      make(chan struct{}),
		lifecycle:  util.NewRunLifecycle(config.Topic+"-consumer", log),
	}, nil
}

// Name returns the name of this consumer group
func (c *consumerImpl) Name() string {
	return c.name
}

// Topics returns the topics that this consumer is subscribed to
func (c *consumerImpl) Topics() []string {
	return []string{c.topic}
}

// Start starts the consumer
func (c *consumerImpl) Start() error {
	return c.lifecycle.Start(func() error {
		go c.eventLoop()
		c.tally.Counter(metrics.KafkaConsumerStarted).Inc(1)
		return nil
	})
}

// Stop stops the consumer
func (c *consumerImpl) Stop() {
	c.lifecycle.Stop(func() {
		c.logger.Info("consumer shutting down", zap.String("topic", c.topic))
		close(c.stopC)
		c.tally.Counter(metrics.KafkaConsumerStopped).Inc(1)
	})
}

// Closed returns a channel which will closed after this consumer is shutown
func (c *consumerImpl) Closed() <-chan struct{} {
	return c.doneC
}

// Messages returns the message channel for this consumer
func (c *consumerImpl) Messages() <-chan kafka.Message {
	return c.msgCh
}

// eventLoop is the main event loop for this consumer
func (c *consumerImpl) eventLoop() {
	c.logger.Info("consumer started", zap.String("topic", c.topic))
	for {
		select {
		case pc := <-c.consumer.Partitions():
			c.addPartition(pc)
		case n := <-c.consumer.Notifications():
			c.handleNotification(n)
		case err := <-c.consumer.Errors():
			c.logger.Error("consumer error", zap.String("topic", c.topic), zap.Error(err))
		case <-c.stopC:
			c.shutdown()
			c.logger.Info("consumer stopped", zap.String("topic", c.topic))
			return
		}
	}
}

// addPartition adds a new partition. If the partition already exist,
// it is first stopped before overwriting it with the new partition
func (c *consumerImpl) addPartition(pc cluster.PartitionConsumer) {
	old := c.partitions.Get(pc.Partition())
	if old != nil {
		old.Stop()
		c.partitions.Delete(pc.Partition())
	}
	c.logger.Info("new partition", zap.String("topic", c.topic), zap.Int32("id", pc.Partition()))
	p := newPartitionConsumer(c.consumer, pc, c.options, c.msgCh, c.dlq, c.tally, c.logger)
	c.partitions.Put(pc.Partition(), p)
	p.Start()
}

// handleNotification is the handler that handles notifications
// from the underlying library about partition rebalances. There
// is no action taken in this handler except for logging.
func (c *consumerImpl) handleNotification(n *cluster.Notification) {
	var ok bool
	var claimed, released, current []int32
	if claimed, ok = n.Claimed[c.topic]; !ok {
		claimed = []int32{}
	}
	if released, ok = n.Released[c.topic]; !ok {
		released = []int32{}
	}
	if current, ok = n.Current[c.topic]; !ok {
		current = []int32{}
	}
	c.logger.Info("cluster rebalance notification", zap.String("topic", c.topic),
		zap.Int32s("claimed", claimed), zap.Int32s("released", released),
		zap.Int32s("current", current))
}

// shutdown shutsdown the consumer
func (c *consumerImpl) shutdown() {
	var wg sync.WaitGroup
	for _, pc := range c.partitions.partitions {
		wg.Add(1)
		go func(p *partitionConsumer) {
			p.Drain(2 * c.options.OffsetCommitInterval)
			wg.Done()
		}(pc)
	}
	wg.Wait()
	c.partitions.Clear()
	c.consumer.CommitOffsets()
	c.consumer.Close()
	c.dlq.Close()
	close(c.doneC)
}

// newPartitionMap returns a partitionMap, a wrapper around a map
func newPartitionMap() partitionMap {
	return partitionMap{
		partitions: make(map[int32]*partitionConsumer, 8),
	}
}

// Get returns the partition with the given id, if it exists
func (m *partitionMap) Get(key int32) *partitionConsumer {
	p, ok := m.partitions[key]
	if !ok {
		return nil
	}
	return p
}

// Delete deletes the partition with the given id
func (m *partitionMap) Delete(key int32) {
	delete(m.partitions, key)
}

// Put adds the partition with the given key
func (m *partitionMap) Put(key int32, value *partitionConsumer) error {
	if m.Get(key) != nil {
		return fmt.Errorf("partition already exist")
	}
	m.partitions[key] = value
	return nil
}

// Clear clears all entries in the map
func (m *partitionMap) Clear() {
	for k := range m.partitions {
		delete(m.partitions, k)
	}
}
