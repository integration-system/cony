package cony

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/streadway/amqp"
)

// ErrPublisherDead indicates that publisher was canceled, could be returned
// from Write() and Publish() methods
var (
	ErrPublisherDead = errors.New("Publisher is dead")
	emptyErr         = atomErr{errors.New("noop")}
)

// PublisherOpt is a functional option type for Publisher
type PublisherOpt func(*Publisher)

type publishMaybeErr struct {
	pub chan amqp.Publishing
	err chan error
	key string
}

type atomErr struct {
	err error
}

// Publisher hold definition for AMQP publishing
type Publisher struct {
	exchange       string
	key            string
	tmpl           amqp.Publishing
	pubChan        chan publishMaybeErr
	stop           chan struct{}
	confirmChan    chan amqp.Confirmation
	dead           bool
	m              sync.Mutex
	lastChannelErr atomic.Value
}

// Template will be used, input buffer will be added as Publishing.Body.
// return int will always be len(b)
//
// Implements io.Writer
//
// WARNING: this is blocking call, it will not return until connection is
// available. The only way to stop it is to use Cancel() method.
func (p *Publisher) Write(b []byte) (int, error) {
	pub := p.tmpl
	pub.Body = b
	return len(b), p.Publish(pub)
}

// PublishWithRoutingKey used to publish custom amqp.Publishing and routing key
//
// WARNING: this is blocking call, it will not return until connection is
// available. The only way to stop it is to use Cancel() method.
func (p *Publisher) PublishWithRoutingKey(pub amqp.Publishing, key string) error {
	if err := p.lastChannelErr.Load(); err != emptyErr {
		if err == nil {
			return errors.New("publisher is not initialized")
		}
		return err.(atomErr).err
	}

	reqRepl := publishMaybeErr{
		pub: make(chan amqp.Publishing, 2),
		err: make(chan error, 2),
		key: key,
	}

	reqRepl.pub <- pub

	select {
	case <-p.stop:
		// received stop signal
		return ErrPublisherDead
	case p.pubChan <- reqRepl:
	}

	err := <-reqRepl.err
	return err
}

// Publish used to publish custom amqp.Publishing
//
// WARNING: this is blocking call, it will not return until connection is
// available. The only way to stop it is to use Cancel() method.
func (p *Publisher) Publish(pub amqp.Publishing) error {
	return p.PublishWithRoutingKey(pub, p.key)
}

// Cancel this publisher
func (p *Publisher) Cancel() {
	p.m.Lock()
	defer p.m.Unlock()

	if !p.dead {
		close(p.stop)
		p.dead = true
	}
}

func (p *Publisher) serve(client owner, ch mqChannel) {
	p.lastChannelErr.Store(emptyErr)
	chanErrs := make(chan *amqp.Error)
	ch.NotifyClose(chanErrs)

	if p.confirmChan != nil {
		if err := ch.Confirm(false); err != nil {
			client.reportErr(err)
		} else {
			p.confirmChan = ch.NotifyPublish(p.confirmChan)
		}

	}

	for {
		select {
		case <-p.stop:
			client.deletePublisher(p)
			ch.Close()
			return
		case err := <-chanErrs:
			if err != nil {
				p.lastChannelErr.Store(atomErr{err})
			}
			return
		case envelop := <-p.pubChan:
			msg := <-envelop.pub
			close(envelop.pub)
			if err := ch.Publish(
				p.exchange,  // exchange
				envelop.key, // key
				false,       // mandatory
				false,       // immediate
				msg,         // msg amqp.Publishing
			); err != nil {
				envelop.err <- err
			}
			close(envelop.err)
		}
	}
}

// NewPublisher is a Publisher constructor
func NewPublisher(exchange string, key string, opts ...PublisherOpt) *Publisher {
	p := &Publisher{
		exchange: exchange,
		key:      key,
		pubChan:  make(chan publishMaybeErr),
		stop:     make(chan struct{}),
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// PublishingTemplate Publisher's functional option. Provide template
// amqp.Publishing and save typing.
func PublishingTemplate(t amqp.Publishing) PublisherOpt {
	return func(p *Publisher) {
		p.tmpl = t
	}
}

func WithConfirmation(confirmChan chan amqp.Confirmation) PublisherOpt {
	return func(p *Publisher) {
		p.confirmChan = confirmChan
	}
}
