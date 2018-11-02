package qcat

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/ghetzel/go-stockutil/typeutil"
	"github.com/ghetzel/go-stockutil/utils"
	"github.com/streadway/amqp"
)

var DefaultQueueName = `qcat`
var DefaultConnectTimeout = 5 * time.Second

type AMQP struct {
	ID                string
	Host              string
	Port              int
	Username          string
	Password          string
	ConnectTimeout    time.Duration
	HeartbeatInterval time.Duration
	TLS               *tls.Config
	Vhost             string
	ExchangeName      string
	RoutingKey        string
	QueueName         string
	Durable           bool
	Autodelete        bool
	Exclusive         bool
	Mandatory         bool
	Immediate         bool
	AutoAck           bool
	Prefetch          int
	Headers           map[string]interface{}
	ClientProperties  map[string]interface{}
	conn              *amqp.Connection
	channel           *amqp.Channel
	queue             amqp.Queue
	uri               amqp.URI
	outchan           chan *Message
	downstreamErrchan chan *amqp.Error
	errchan           chan error
}

type DeliveryMode int

const (
	Transient  DeliveryMode = 1
	Persistent              = 2
)

type MessageHeader struct {
	ContentType     string
	ContentEncoding string
	DeliveryMode    DeliveryMode
	Priority        int
	Expiration      time.Duration
}

type Message struct {
	Timestamp   time.Time
	Header      MessageHeader
	Body        []byte
	delivery    *amqp.Delivery
	ackRequired bool
}

func (self *Message) ShouldAck() bool {
	return self.ackRequired
}

// Acknowledge the successful processing of a message.
func (self *Message) Acknowledge() error {
	if self.ShouldAck() {
		return self.delivery.Ack(false)
	} else {
		return nil
	}
}

// Reject a message, but don't requeue it.
func (self *Message) Reject() error {
	if self.ShouldAck() {
		return self.delivery.Nack(false, false)
	} else {
		return nil
	}
}

// Reject a message and requeue it.
func (self *Message) Requeue() error {
	if self.ShouldAck() {
		return self.delivery.Nack(false, true)
	} else {
		return nil
	}
}

func (self *Message) Decode(into interface{}) error {
	switch self.Header.ContentType {
	case `application/json`:
		return json.Unmarshal(self.Body, into)
	default:
		if b, ok := into.([]byte); ok {
			if n := copy(b, self.Body); n == 0 && len(self.Body) > 0 {
				return fmt.Errorf("target must be able to hold at least %d bytes", len(self.Body))
			} else {
				return nil
			}
		} else {
			return typeutil.SetValue(into, string(self.Body))
		}
	}
}

func NewAMQP(uri string) (*AMQP, error) {
	c := &AMQP{
		QueueName:         DefaultQueueName,
		Headers:           make(map[string]interface{}),
		AutoAck:           true,
		ConnectTimeout:    DefaultConnectTimeout,
		ClientProperties:  make(map[string]interface{}),
		outchan:           make(chan *Message),
		downstreamErrchan: make(chan *amqp.Error),
		errchan:           make(chan error),
	}

	if u, err := amqp.ParseURI(uri); err == nil {
		c.uri = u
		c.Host = u.Host
		c.Port = u.Port
		c.Username = u.Username
		c.Password = u.Password
		c.Vhost = u.Vhost

		return c, nil
	} else {
		return nil, err
	}
}

func (self *AMQP) Close() error {
	var merr error

	if self.conn == nil {
		return fmt.Errorf("Cannot close, connection does not exist")
	} else if self.channel != nil {
		if err := self.channel.Cancel(self.ID, false); err != nil {
			merr = utils.AppendError(merr, err)
		}

		merr = utils.AppendError(merr, self.channel.Close())
	}

	return utils.AppendError(merr, self.conn.Close())
}

func (self *AMQP) Connect() error {
	if _, ok := self.ClientProperties[`product`]; !ok {
		self.ClientProperties[`product`] = `qcat`
		self.ClientProperties[`version`] = Version
	}

	if _, ok := self.ClientProperties[`hostname`]; !ok {
		if hostname, err := os.Hostname(); err == nil {
			self.ClientProperties[`hostname`] = hostname
		}
	}

	if conn, err := amqp.DialConfig(self.uri.String(), amqp.Config{
		TLSClientConfig: self.TLS,
		Properties:      amqp.Table(self.ClientProperties),
		Heartbeat:       self.HeartbeatInterval,
		Dial: func(network, addr string) (net.Conn, error) {
			return net.DialTimeout(network, addr, self.ConnectTimeout)
		},
	}); err == nil {
		self.conn = conn

		if channel, err := self.conn.Channel(); err == nil {
			if err := channel.Qos(self.Prefetch, 0, false); err != nil {
				return err
			}

			self.channel = channel

			// setup error notifications
			go func() {
				for qerr := range self.channel.NotifyClose(self.downstreamErrchan) {
					if qerr.Server {
						self.errchan <- fmt.Errorf("server error %d: %v", qerr.Code, qerr.Reason)
					} else {
						self.errchan <- fmt.Errorf("client error %d: %v", qerr.Code, qerr.Reason)
					}
				}
			}()

			//  declare queue
			if self.QueueName != `` {
				if queue, err := self.channel.QueueDeclare(
					self.QueueName,
					self.Durable,
					self.Autodelete,
					self.Exclusive,
					false,
					amqp.Table(self.Headers),
				); err == nil {
					self.queue = queue
					return nil
				} else {
					defer self.channel.Close()
					return err
				}
			}
		} else {
			defer self.conn.Close()
			return err
		}
	} else {
		return err
	}

	return nil
}

func (self *AMQP) SubscribeRaw() (<-chan amqp.Delivery, error) {
	return self.channel.Consume(
		self.queue.Name,
		self.ID,
		self.AutoAck,
		self.Exclusive,
		false,
		false,
		amqp.Table(self.Headers),
	)
}

// Publish messages read from the given reader, separated by newlines ("\n").
func (self *AMQP) PublishLines(reader io.Reader, header MessageHeader) error {
	inScanner := bufio.NewScanner(reader)

	for inScanner.Scan() {
		if err := self.Publish([]byte(inScanner.Bytes()), header); err != nil {
			return err
		}
	}

	return inScanner.Err()
}

// Publish a single message.
func (self *AMQP) Publish(data []byte, header MessageHeader) error {
	var deliveryMode int

	switch header.DeliveryMode {
	case Transient:
		deliveryMode = 1
	case Persistent:
		deliveryMode = 2
	}

	pubOpts := amqp.Publishing{
		Body:            data,
		ContentType:     header.ContentType,
		ContentEncoding: header.ContentEncoding,
		DeliveryMode:    uint8(deliveryMode),
		Priority:        uint8(header.Priority),
		Timestamp:       time.Now(),
	}

	if header.Expiration > 0 {
		pubOpts.Expiration = fmt.Sprintf("%d", int(
			header.Expiration.Round(time.Millisecond)/time.Millisecond,
		))
	}

	return self.channel.Publish(self.ExchangeName, self.RoutingKey, self.Mandatory, self.Immediate, pubOpts)
}

// Receive a message from the channel.
func (self *AMQP) Subscribe() error {
	if msgs, err := self.SubscribeRaw(); err == nil {
		go func() {
			for delivery := range msgs {
				var deliveryMode DeliveryMode

				switch delivery.DeliveryMode {
				case 2:
					deliveryMode = Persistent
				default:
					deliveryMode = Transient
				}

				self.outchan <- &Message{
					delivery:    &delivery,
					ackRequired: !self.AutoAck,
					Timestamp:   delivery.Timestamp,
					Body:        delivery.Body,
					Header: MessageHeader{
						ContentType:     delivery.ContentType,
						ContentEncoding: delivery.ContentEncoding,
						DeliveryMode:    deliveryMode,
						Priority:        int(delivery.Priority),
					},
				}
			}

			close(self.outchan)
		}()

		return nil
	} else {
		return err
	}
}

// Receive a single message.
func (self *AMQP) Receive() <-chan *Message {
	return self.outchan
}

// Receive a single error.
func (self *AMQP) Err() <-chan error {
	return self.errchan
}
