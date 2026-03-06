package client

import (
	"fmt"
	"sync"
)

type PluginClient[T any] struct {
	mu        sync.Mutex
	client    T
	cleanup   func()
	connect   func() (T, func(), error)
	onConnect func(T) error
}

func NewPluginClient[T any](connect func() (T, func(), error)) *PluginClient[T] {
	return &PluginClient[T]{connect: connect}
}

func (p *PluginClient[T]) OnConnect(fn func(T) error) {
	p.onConnect = fn
}

func (p *PluginClient[T]) Client() (T, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cleanup != nil {
		return p.client, nil
	}

	return p.connectLocked()
}

func (p *PluginClient[T]) Reconnect() (T, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cleanup != nil {
		p.cleanup()
		p.cleanup = nil
	}

	return p.connectLocked()
}

func (p *PluginClient[T]) connectLocked() (T, error) {
	client, cleanup, err := p.connect()
	if err != nil {
		var zero T
		return zero, fmt.Errorf("connecting to plugin: %w", err)
	}

	if p.onConnect != nil {
		if err := p.onConnect(client); err != nil {
			cleanup()
			var zero T
			return zero, fmt.Errorf("onConnect callback: %w", err)
		}
	}

	p.client = client
	p.cleanup = cleanup
	return p.client, nil
}

func (p *PluginClient[T]) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cleanup != nil {
		p.cleanup()
		p.cleanup = nil
	}
}
