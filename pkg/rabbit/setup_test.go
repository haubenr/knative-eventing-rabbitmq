/*
Copyright 2022 The Knative Authors

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

package rabbit

import (
	"testing"

	"go.uber.org/zap"
)

func Test_SetupRabbitMQReconnections(t *testing.T) {
	rabbitMQHelper := NewRabbitMQHelper(2, make(chan bool))
	logger := zap.NewNop().Sugar()
	testChannel := make(chan bool)
	// Drain messages in the retryChannel
	go func() {
		testChannel <- true
		for {
			retry := rabbitMQHelper.WaitForRetrySignal()
			if !retry {
				close(testChannel)
				break
			}
			testChannel <- retry
		}
	}()
	<-testChannel
	// Testing a failing setup
	conn, _, err := rabbitMQHelper.SetupRabbitMQ("amqp://localhost:5672/%2f", logger)
	<-testChannel
	if err == nil {
		t.Error("SetupRabbitMQ should fail with the default DialFunc in testing environments")
	}
	if rabbitMQHelper.retryCounter == 0 {
		t.Errorf("no retries have been attempted want: > 0, got: %d", rabbitMQHelper.retryCounter)
	}
	// Test SignalRetry func
	rabbitMQHelper.SignalRetry(true)
	<-testChannel
	rabbitMQHelper.CleanupRabbitMQ(conn, logger)
}
