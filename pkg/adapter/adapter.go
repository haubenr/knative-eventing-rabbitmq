/*
Copyright 2020 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package rabbitmq

import (
	"context"
	"fmt"
	nethttp "net/http"
	"strings"
	"sync"

	"github.com/NeowayLabs/wabbit"
	"github.com/NeowayLabs/wabbit/amqp"
	"github.com/NeowayLabs/wabbit/amqptest"

	"go.uber.org/zap"

	"knative.dev/eventing-rabbitmq/pkg/rabbit"
	"knative.dev/eventing-rabbitmq/pkg/utils"
	"knative.dev/eventing/pkg/adapter/v2"
	v1 "knative.dev/eventing/pkg/apis/duck/v1"
	"knative.dev/eventing/pkg/kncloudevents"
	"knative.dev/eventing/pkg/metrics/source"
	"knative.dev/pkg/logging"
)

const resourceGroup = "rabbitmqsources.sources.knative.dev"

type adapterConfig struct {
	adapter.EnvConfig

	RabbitURL     string `envconfig:"RABBIT_URL" required:"true"`
	Vhost         string `envconfig:"RABBITMQ_VHOST" required:"false"`
	Predeclared   bool   `envconfig:"RABBITMQ_PREDECLARED" required:"false"`
	Retry         int    `envconfig:"HTTP_SENDER_RETRY" required:"false"`
	BackoffPolicy string `envconfig:"HTTP_SENDER_BACKOFF_POLICY" required:"false"`
	BackoffDelay  string `envconfig:"HTTP_SENDER_BACKOFF_DELAY" default:"50ms" required:"false"`
	Parallelism   int    `envconfig:"RABBITMQ_CHANNEL_PARALLELISM" default:"1" required:"false"`
	ExchangeName  string `envconfig:"RABBITMQ_EXCHANGE_NAME" required:"false"`
	QueueName     string `envconfig:"RABBITMQ_QUEUE_NAME" required:"true"`
}

func NewEnvConfig() adapter.EnvConfigAccessor {
	return &adapterConfig{}
}

type Adapter struct {
	config            *adapterConfig
	httpMessageSender *kncloudevents.HTTPMessageSender
	reporter          source.StatsReporter
	logger            *zap.Logger
	context           context.Context
}

var _ adapter.MessageAdapter = (*Adapter)(nil)
var _ adapter.MessageAdapterConstructor = NewAdapter
var (
	retryConfig  kncloudevents.RetryConfig = kncloudevents.NoRetries()
	retriesInt32 int32                     = 0
)

func NewAdapter(ctx context.Context, processed adapter.EnvConfigAccessor, httpMessageSender *kncloudevents.HTTPMessageSender, reporter source.StatsReporter) adapter.MessageAdapter {
	logger := logging.FromContext(ctx).Desugar()
	config := processed.(*adapterConfig)
	return &Adapter{
		config:            config,
		httpMessageSender: httpMessageSender,
		reporter:          reporter,
		logger:            logger,
		context:           ctx,
	}
}

func vhostHandler(broker string, vhost string) string {
	if len(vhost) > 0 && len(broker) > 0 && !strings.HasSuffix(broker, "/") &&
		!strings.HasPrefix(vhost, "/") {
		return fmt.Sprintf("%s/%s", broker, vhost)
	}

	return fmt.Sprintf("%s%s", broker, vhost)
}

func (a *Adapter) CreateConn(logger *zap.Logger) (wabbit.Conn, error) {
	conn, err := amqp.Dial(vhostHandler(a.config.RabbitURL, a.config.Vhost))
	if err != nil {
		logger.Error(err.Error())
	}
	return conn, err
}

func (a *Adapter) CreateChannel(conn wabbit.Conn, connTest *amqptest.Conn,
	logger *zap.Logger) (wabbit.Channel, error) {
	var ch wabbit.Channel
	var err error

	if conn != nil {
		ch, err = conn.Channel()
	} else {
		ch, err = connTest.Channel()
	}
	if err != nil {
		logger.Error(err.Error())
		return nil, err
	}

	logger.Info("Initializing Channel with Config: ",
		zap.Int("Parallelism", a.config.Parallelism),
	)

	err = ch.Qos(
		a.config.Parallelism,
		0,
		false,
	)

	return ch, err
}

func (a *Adapter) Start(ctx context.Context) error {
	return a.start(ctx.Done())
}

func (a *Adapter) start(stopCh <-chan struct{}) error {
	logger := a.logger
	logger.Info("Starting with config: ",
		zap.String("Name", a.config.Name),
		zap.String("Namespace", a.config.Namespace),
		zap.String("QueueName", a.config.QueueName),
		zap.String("SinkURI", a.config.Sink))

	conn, err := a.CreateConn(logger)
	if err == nil {
		defer conn.Close()
	}

	ch, err := a.CreateChannel(conn, nil, logger)
	if err == nil {
		defer ch.Close()
	}

	queue, err := a.StartAmqpClient(&ch)
	if err != nil {
		logger.Error(err.Error())
	}

	return a.PollForMessages(&ch, queue, stopCh)
}

func (a *Adapter) StartAmqpClient(ch *wabbit.Channel) (*wabbit.Queue, error) {
	queue, err := (*ch).QueueInspect(a.config.QueueName)
	return &queue, err
}

func (a *Adapter) ConsumeMessages(channel *wabbit.Channel,
	queue *wabbit.Queue, logger *zap.Logger) (<-chan wabbit.Delivery, error) {
	msgs, err := (*channel).Consume(
		(*queue).Name(),
		"",
		wabbit.Option{
			"autoAck":   false,
			"exclusive": false,
			"noLocal":   false,
			"noWait":    false,
		})

	if err != nil {
		logger.Error(err.Error())
	}
	return msgs, err
}

func (a *Adapter) PollForMessages(channel *wabbit.Channel,
	queue *wabbit.Queue, stopCh <-chan struct{}) error {
	logger := a.logger
	var err error
	if a.config.BackoffDelay != "" {
		retriesInt32 = int32(a.config.Retry)
		backoffPolicy := utils.SetBackoffPolicy(a.context, a.config.BackoffPolicy)
		if backoffPolicy == "" {
			a.logger.Sugar().Fatalf("Invalid BACKOFF_POLICY specified: must be %q or %q", v1.BackoffPolicyExponential, v1.BackoffPolicyLinear)
		}
		retryConfig, err = kncloudevents.RetryConfigFromDeliverySpec(v1.DeliverySpec{
			BackoffPolicy: &backoffPolicy,
			BackoffDelay:  &a.config.BackoffDelay,
			Retry:         &retriesInt32,
		})
		if err != nil {
			a.logger.Error("error retrieving retryConfig from deliverySpec", zap.Error(err))
		}
	}

	msgs, _ := a.ConsumeMessages(channel, queue, logger)
	wg := &sync.WaitGroup{}
	workerCount := a.config.Parallelism
	wg.Add(workerCount)
	workerQueue := make(chan wabbit.Delivery, workerCount)
	logger.Info("Starting GoRoutines Workers: ", zap.Int("WorkerCount", workerCount))

	for i := 0; i < workerCount; i++ {
		go a.processMessages(wg, workerQueue)
	}

	for {
		select {
		case <-stopCh:
			close(workerQueue)
			wg.Wait()
			logger.Info("Shutting down...")
			return nil
		case msg, ok := <-msgs:
			if !ok {
				close(workerQueue)
				wg.Wait()
				return nil
			}
			workerQueue <- msg
		}
	}
}

func (a *Adapter) processMessages(wg *sync.WaitGroup, queue <-chan wabbit.Delivery) {
	defer wg.Done()
	for msg := range queue {
		a.logger.Info("Received: ", zap.String("MessageId", msg.MessageId()))
		if err := a.postMessage(msg); err == nil {
			a.logger.Info("Successfully sent event to sink")
			err = msg.Ack(false)
			if err != nil {
				a.logger.Error("sending Ack failed with Delivery Tag")
			}
		} else {
			a.logger.Error("sending event to sink failed: ", zap.Error(err))
			err = msg.Nack(false, false)
			if err != nil {
				a.logger.Error("sending Nack failed with Delivery Tag")
			}
		}
	}
}

func (a *Adapter) postMessage(msg wabbit.Delivery) error {
	a.logger.Info("target: " + a.httpMessageSender.Target)
	req, err := a.httpMessageSender.NewCloudEventRequest(a.context)
	if err != nil {
		return err
	}

	err = rabbit.ConvertMessageToHTTPRequest(
		a.context,
		a.config.Name,
		a.config.Namespace,
		a.config.QueueName,
		msg,
		req,
		a.logger)
	if err != nil {
		a.logger.Error("error writing event to http", zap.Error(err))
		return err
	}

	res, err := a.httpMessageSender.SendWithRetries(req, &retryConfig)
	if err != nil {
		a.logger.Error("error while sending the message", zap.Error(err))
		return err
	}

	if res.StatusCode/100 != 2 {
		a.logger.Error("unexpected status code", zap.Int("status code", res.StatusCode))
		return fmt.Errorf("%d %s", res.StatusCode, nethttp.StatusText(res.StatusCode))
	}

	reportArgs := &source.ReportArgs{
		Namespace:     a.config.Namespace,
		Name:          a.config.Name,
		ResourceGroup: resourceGroup,
	}

	_ = a.reporter.ReportEventCount(reportArgs, res.StatusCode)
	return nil
}
