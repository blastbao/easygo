// +build linux

package netpoll

import (
	"os"
)

// New creates new epoll-based Poller instance with given config.
func New(c *Config) (Poller, error) {
	cfg := c.withDefaults()

	epoll, err := EpollCreate(&EpollConfig{
		OnWaitError: cfg.OnWaitError,
	})
	if err != nil {
		return nil, err
	}

	return poller{epoll}, nil
}

// poller implements Poller interface.
type poller struct {
	*Epoll
}

// Start implements Poller.Start() method.
func (ep poller) Start(desc *Desc, cb CallbackFn) error {

	// 监听 desc.fd() 上的 desc.event 事件。
	err := ep.Add(desc.fd(), toEpollEvent(desc.event),

		// 回调函数
		func(ep EpollEvent) {
			var event Event

			if ep&EPOLLHUP != 0 {
				event |= EventHup
			}
			if ep&EPOLLRDHUP != 0 {
				event |= EventReadHup
			}
			if ep&EPOLLIN != 0 {
				event |= EventRead
			}
			if ep&EPOLLOUT != 0 {
				event |= EventWrite
			}
			if ep&EPOLLERR != 0 {
				event |= EventErr
			}
			if ep&_EPOLLCLOSED != 0 {
				event |= EventPollerClosed
			}
			cb(event)
		},
	)

	if err == nil {
		// 通过系统调用设置 fd 为非阻塞
		if err = setNonblock(desc.fd(), true); err != nil {
			return os.NewSyscallError("setnonblock", err)
		}
	}
	return err
}

// Stop implements Poller.Stop() method.
func (ep poller) Stop(desc *Desc) error {
	return ep.Del(desc.fd())
}

// Resume implements Poller.Resume() method.
func (ep poller) Resume(desc *Desc) error {
	return ep.Mod(desc.fd(), toEpollEvent(desc.event))
}

func toEpollEvent(event Event) (ep EpollEvent) {

	// 可读事件
	if event&EventRead != 0 {
		ep |= EPOLLIN | EPOLLRDHUP
	}

	// 可写事件
	if event&EventWrite != 0 {
		ep |= EPOLLOUT
	}

	// ET模式
	if event&EventOneShot != 0 {
		ep |= EPOLLONESHOT
	}

	//
	if event&EventEdgeTriggered != 0 {
		ep |= EPOLLET
	}
	return ep
}
