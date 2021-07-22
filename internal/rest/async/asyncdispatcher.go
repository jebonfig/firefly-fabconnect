// Copyright 2021 Kaleido

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at

//     http://www.apache.org/licenses/LICENSE-2.0

// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package async

import (
	"context"
	"net/http"
	"reflect"

	"github.com/hyperledger-labs/firefly-fabconnect/internal/conf"
	"github.com/hyperledger-labs/firefly-fabconnect/internal/errors"
	"github.com/hyperledger-labs/firefly-fabconnect/internal/messages"
	"github.com/hyperledger-labs/firefly-fabconnect/internal/rest/receipt"
	"github.com/hyperledger-labs/firefly-fabconnect/internal/tx"
	"github.com/hyperledger-labs/firefly-fabconnect/internal/utils"
	"github.com/julienschmidt/httprouter"
	log "github.com/sirupsen/logrus"
)

// AsyncDispatcher is passed in to process messages over a streaming system with
// a receipt store. Only used for POST methods, when fly-sync is not set to true
type AsyncDispatcher interface {
	Run() error
	IsInitialized() bool
	DispatchMsgAsync(ctx context.Context, msg map[string]interface{}, ack bool) (*messages.AsyncSentMsg, error)
	HandleReceipts(res http.ResponseWriter, req *http.Request, params httprouter.Params)
}

// Interface to be implemented by the direct handler and kafka-based handler
type asyncRequestHandler interface {
	validateHandlerConf() error
	dispatchMsg(ctx context.Context, key, msgID string, msg map[string]interface{}, ack bool) (msgAck string, statusCode int, err error)
	run() error
	isInitialized() bool
}

type asyncDispatcher struct {
	handler      asyncRequestHandler
	receiptStore receipt.ReceiptStore
}

func NewAsyncDispatcher(conf conf.RESTGatewayConf, processor tx.TxProcessor, receiptstore receipt.ReceiptStore) AsyncDispatcher {
	var handler asyncRequestHandler
	if len(conf.Kafka.Brokers) > 0 {
		handler = newKafkaHandler(conf.Kafka, receiptstore)
	} else {
		handler = newDirectHandler(&conf, processor, receiptstore)
	}

	return &asyncDispatcher{
		handler:      handler,
		receiptStore: receiptstore,
	}
}

// DispatchMsgAsync is the interface method for async dispatching of messages
func (d *asyncDispatcher) DispatchMsgAsync(ctx context.Context, msg map[string]interface{}, ack bool) (*messages.AsyncSentMsg, error) {
	reply, _, err := d.processMsg(ctx, msg, ack)
	return reply, err
}

func (d *asyncDispatcher) HandleReceipts(res http.ResponseWriter, req *http.Request, params httprouter.Params) {
	path := req.URL.Path
	if path == "receipts" {
		d.receiptStore.GetReceipts(res, req, params)
	} else {
		d.receiptStore.GetReceipt(res, req, params)
	}
}

func (w *asyncDispatcher) processMsg(ctx context.Context, msg map[string]interface{}, ack bool) (*messages.AsyncSentMsg, int, error) {
	// Check we understand the type, and can get the key.
	// The rest of the validation is performed by the bridge listening to Kafka
	headers, exists := msg["headers"]
	if !exists || reflect.TypeOf(headers).Kind() != reflect.Map {
		return nil, 400, errors.Errorf(errors.RequestHandlerInvalidMsgHeaders)
	}
	msgType, exists := headers.(map[string]interface{})["type"]
	if !exists || reflect.TypeOf(msgType).Kind() != reflect.String {
		return nil, 400, errors.Errorf(errors.RequestHandlerInvalidMsgTypeMissing)
	}
	var key string
	switch msgType {
	case messages.MsgTypeDeployContract, messages.MsgTypeSendTransaction:
		from, exists := msg["from"]
		if !exists || reflect.TypeOf(from).Kind() != reflect.String {
			return nil, 400, errors.Errorf(errors.RequestHandlerInvalidMsgFromMissing)
		}
		key = from.(string)
	default:
		return nil, 400, errors.Errorf(errors.RequestHandlerInvalidMsgType, msgType)
	}

	// We always generate the ID. It cannot be set by the user
	msgID := utils.UUIDv4()
	headers.(map[string]interface{})["id"] = msgID

	// Pass to the handler
	log.Infof("Request handler accepted message. MsgID: %s Type: %s", msgID, msgType)
	msgAck, status, err := w.handler.dispatchMsg(ctx, key, msgID, msg, ack)
	if err != nil {
		return nil, status, err
	}
	return &messages.AsyncSentMsg{
		Sent:    true,
		Request: msgID,
		Msg:     msgAck,
	}, 200, nil
}

func (w *asyncDispatcher) Run() error {
	return w.handler.run()
}

func (w *asyncDispatcher) IsInitialized() bool {
	return w.handler.isInitialized()
}
