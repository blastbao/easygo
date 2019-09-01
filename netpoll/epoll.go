// +build linux

package netpoll

import (
	"sync"

	"golang.org/x/sys/unix"
)




// EpollEvent represents epoll events configuration bit mask.
// EpollEvent 表示 epoll 事件的配置掩码
type EpollEvent uint32


// EpollEvents that are mapped to epoll_event.events possible values.
const (
	EPOLLIN      = unix.EPOLLIN
	EPOLLOUT     = unix.EPOLLOUT
	EPOLLRDHUP   = unix.EPOLLRDHUP
	EPOLLPRI     = unix.EPOLLPRI
	EPOLLERR     = unix.EPOLLERR
	EPOLLHUP     = unix.EPOLLHUP
	EPOLLET      = unix.EPOLLET
	EPOLLONESHOT = unix.EPOLLONESHOT

	// _EPOLLCLOSED is a special EpollEvent value the receipt of which means
	// that the epoll instance is closed.
	_EPOLLCLOSED = 0x20
)



// String returns a string representation of EpollEvent.
func (evt EpollEvent) String() (str string) {

	name := func(event EpollEvent, name string) {
		// 检查 evt 第 event 位是否被置位。
		if evt&event == 0 {
			return
		}
		if str != "" {
			str += "|"
		}
		str += name
	}


	// 根据对应位是否被置位，决定是否拼接对应字符串

	name(EPOLLIN, "EPOLLIN")
	name(EPOLLOUT, "EPOLLOUT")
	name(EPOLLRDHUP, "EPOLLRDHUP")
	name(EPOLLPRI, "EPOLLPRI")
	name(EPOLLERR, "EPOLLERR")
	name(EPOLLHUP, "EPOLLHUP")
	name(EPOLLET, "EPOLLET")
	name(EPOLLONESHOT, "EPOLLONESHOT")
	name(_EPOLLCLOSED, "_EPOLLCLOSED")

	return
}





// Epoll represents single epoll instance.
type Epoll struct {

	mu sync.RWMutex

	fd       int
	eventFd  int
	closed   bool
	waitDone chan struct{}

	// map<fd> -> callback()
	callbacks map[int]func(EpollEvent)
}




// EpollConfig contains options for Epoll instance configuration.
type EpollConfig struct {

	// OnWaitError will be called from goroutine, waiting for events.
	OnWaitError func(error)

}



//
func (c *EpollConfig) withDefaults() (config EpollConfig) {
	if c != nil {
		config = *c
	}
	if config.OnWaitError == nil {
		config.OnWaitError = defaultOnWaitError
	}
	return config
}




// EpollCreate creates new epoll instance.
// It starts the wait loop in separate goroutine.
func EpollCreate(c *EpollConfig) (*Epoll, error) {


	// config 主要是设置 OnWaitError 函数
	config := c.withDefaults()

	// 创建 epoll fd
	fd, err := unix.EpollCreate1(0)
	if err != nil {
		return nil, err
	}





	// eventFd 是 Linux 系统后来才引入的一种轻量级的 IPC 方式，不同进程可以通过 eventfd 机制建立起一个共享的计数器，
	// 这个计数器由内核负责维护，充当了信息的角色，与它关联的进程可以对其进行读写，从而起到进程间通讯的目的。
	//
	//
	// 相关方法：
	//	1. eventfd(): 创建一个 eventFd，返回一个文件描述符，通过该文件描述符可对 eventFd 进行读写操作。
	//
	//	2. read(): 读取计数器中的值。
	//		2.1 计数器中的值大于 0 ———— 非初始状态
	//			2.1.1 设置了 EFD_SEMAPHORE 标志，返回 1 ，且计数器中的值减 1 ，这个模式是为跨进程做递减标记等而设计及的。
	//			2.1.2 没有设置 EFD_SEMAPHORE 标志，返回计数器中的值，计数器清零。
	//		2.2 计数器中的值等于 0 ，说明没有人写入值，初始状态默认为 0 。
	//			2.2.1 设置了 EFD_NOBLOCK 标志，返回 -1 ，不阻塞。
	//			2.2.2 未设置 EFD_NOBLOCK 标志，阻塞直到计数器中的值大于 0 。
	//
	//	3. write(): 向计数器写值，写入的值会和原先计数器中的值累加。
	//		3.1 写入的值和原先计数器中的值的和小于 0xFFFFFFFFFFFFFFFE ，写入成功。
	//		3.2 否则:
	//			3.2.1 设置了 EFD_NONBLOCK 标志，返回 -1 。
	//			3.2.2 没有设置，阻塞直到 read 。
	//
	//	4. close(): 关闭
	//
	// eventFd 在内核中关联一个对应的 struct file 结构，这个 struct file 结构是 eventFd 的精髓，
	// 实际上它既不对应硬盘上的一个真实文件，也不占用 inode ，它是对内存中一个数据结构的抽象，
	// 这个数据结构的核心是一个64位无符号型整数。
	//
	// 有了这个结构，我们就可以像操作普通文件一样在 eventFd 描述符上进行 read() 和 write() 操作了。
	// 简单来说，read() 操作读 count 值，write() 操作写 count 值。再结合其他的一些限制条件，还可以实现阻塞/非阻塞的模型。
	// 这样我们就可以用 eventFd 来实现最基本的进程间通信功能了，man eventfd 里就给出了一个非常直观的例子，
	// 并且说明当仅用于实现信号通知的功能时，eventfd() 完全可以替代 pipe() ，对于内核来说，eventfd 的开销更低，
	// 消耗的文件描述符数目更少（ eventfd 只占用一个描述符，而 pipe 则需要两个）。
	//
	// eventFd 不仅仅支持进程间的通信，而且可以作为用户进程和内核通信的桥梁，fs/eventfd.c 中提供的 eventfd_signal() 函数就
	// 是通过在事件发生时将 eventFd 标记为可读，从而达到通知用户进程的目的。
	// man手册中也对此特别进行了说明，并提到 Linux 内核 aio（KAIO）就支持了这种用法，用于将读写操作完成的事件通知到应用程序。
	//
	// 采用 eventfd 还有一个好处就是可以像对待其它文件描述符一样使用多路复用（select、poll、epoll）机制来对它进行监控，
	// 这样在采用了 AIO 机制的程序中就不用阻塞在某个特定的描述符上了。
	//
	// eventFd 其实是内核为应用程序提供的信号量，用户空间的应用程序可以用这个 eventFd 来实现事件的等待或通知机制，
	// 也可以用于内核通知新的事件到用户空间应用程序。
	//
	// eventFd 相比于 POSIX 信号量的优势是，在内核里以文件形式存在，可用于 select/epoll 循环中，因此可以实现异步的信号量，
	// 避免了消费者在资源不可用时的阻塞，这也是为什么取名叫 eventFd 的原因：event 表示它可用来作事件通知（当然是异步的），
	// fd 表示它是一个“文件”。
	//
	//
	// 用户可以通过监听 eventFd 上的可读事件来实现事件通知。

	r0, _, errno := unix.Syscall(unix.SYS_EVENTFD2, 0, 0, 0)
	if errno != 0 {
		return nil, errno
	}
	eventFd := int(r0)


	// Set finalizer for write end of socket pair to avoid data races when closing Epoll
	// instance and EBADF errors on writing ctl bytes from callers.
	//
	// 将 eventFd 的可读事件监听注册到 fd 上，这里 eventFd 的作用是广播关闭信号。
	err = unix.EpollCtl(fd, unix.EPOLL_CTL_ADD, eventFd, &unix.EpollEvent{
		Events: unix.EPOLLIN,
		Fd:     int32(eventFd),
	})
	if err != nil {
		unix.Close(fd)
		unix.Close(eventFd)
		return nil, err
	}



	ep := &Epoll{
		fd:        fd,
		eventFd:   eventFd,
		callbacks: make(map[int]func(EpollEvent)),
		waitDone:  make(chan struct{}),
	}



	// Run wait loop.
	go ep.wait(config.OnWaitError)

	return ep, nil
}

// closeBytes used for writing to eventfd.
var closeBytes = []byte{1, 0, 0, 0, 0, 0, 0, 0}

// Close stops wait loop and closes all underlying resources.
func (ep *Epoll) Close() (err error) {

	ep.mu.Lock()
	{
		if ep.closed {
			ep.mu.Unlock()
			return ErrClosed
		}

		ep.closed = true

		// 通过 ep.eventFd 广播关闭信号，wait() 函数中会监听 ep.eventFd 上的关闭广播信号，然后退出 wait()。
		if _, err = unix.Write(ep.eventFd, closeBytes); err != nil {
			ep.mu.Unlock()
			return
		}
	}

	ep.mu.Unlock()

	// 阻塞等待 wait() 退出，它退出时会 close(ep.waitDone) 广播关闭信号。
	<-ep.waitDone
	if err = unix.Close(ep.eventFd); err != nil {
		return
	}

	ep.mu.Lock()

	// Set callbacks to nil preventing long mu.Lock() hold.
	// This could increase the speed of retreiving ErrClosed in other calls to current epoll instance.
	// Setting callbacks to nil is safe here because no one should read after closed flag is true.
	callbacks := ep.callbacks 	// 暂存所有回调。
	ep.callbacks = nil			// 析构。
	ep.mu.Unlock()

	// 关闭也是一种事件，发送给每个回调函数进行处理。
	for _, cb := range callbacks {
		if cb != nil {
			cb(_EPOLLCLOSED)
		}
	}

	return
}

// Add adds fd to epoll set with given events.
// Callback will be called on each received event from epoll.
// Note that _EPOLLCLOSED is triggered for every cb when epoll closed.
//
// 注册 fd 的监听事件 events 和 事件回调 到 epoll 里。
func (ep *Epoll) Add(fd int, events EpollEvent, cb func(EpollEvent)) (err error) {

	// 构造监听事件结构体
	ev := &unix.EpollEvent{
		Events: uint32(events),
		Fd:     int32(fd),
	}

	ep.mu.Lock()
	defer ep.mu.Unlock()

	// 检查 epollFD 是否关闭
	if ep.closed {
		return ErrClosed
	}

	// 注册事件回调函数
	if _, has := ep.callbacks[fd]; has {
		return ErrRegistered
	}
	ep.callbacks[fd] = cb

	// 执行 fd 监听事件注册
	return unix.EpollCtl(ep.fd, unix.EPOLL_CTL_ADD, fd, ev)
}

// Del removes fd from epoll set.
func (ep *Epoll) Del(fd int) (err error) {
	ep.mu.Lock()
	defer ep.mu.Unlock()

	if ep.closed {
		return ErrClosed
	}
	if _, ok := ep.callbacks[fd]; !ok {
		return ErrNotRegistered
	}
	delete(ep.callbacks, fd)

	return unix.EpollCtl(ep.fd, unix.EPOLL_CTL_DEL, fd, nil)
}

// Mod sets to listen events on fd.
func (ep *Epoll) Mod(fd int, events EpollEvent) (err error) {

	ev := &unix.EpollEvent{
		Events: uint32(events),
		Fd:     int32(fd),
	}

	ep.mu.RLock()
	defer ep.mu.RUnlock()

	if ep.closed {
		return ErrClosed
	}
	if _, ok := ep.callbacks[fd]; !ok {
		return ErrNotRegistered
	}

	return unix.EpollCtl(ep.fd, unix.EPOLL_CTL_MOD, fd, ev)
}


const (
	maxWaitEventsBegin = 1024
	maxWaitEventsStop  = 32768
)

func (ep *Epoll) wait(onError func(error)) {

	defer func() {
		if err := unix.Close(ep.fd); err != nil {
			onError(err)
		}
		close(ep.waitDone)
	}()

	// 用于保存 unix.EpollWait 上的触发事件
	events := make([]unix.EpollEvent, maxWaitEventsBegin)
	// 用于保存 unix.EpollWait 上触发事件对应的回调函数，这些喊出此前通过 ep.Add() 注册在 ep.callbacks[] 的 map 中。
	callbacks := make([]func(EpollEvent), 0, maxWaitEventsBegin)

	for {

		// 阻塞式 epoll_wait 系统调用
		n, err := unix.EpollWait(ep.fd, events, -1)
		if err != nil {
			// 临时性错误则 continue
			if temporaryErr(err) {
				continue
			}
			onError(err)
			return
		}

		// 构造 callbacks[] 切片，保存每个已触发事件的回调函数。
		callbacks = callbacks[:n]

		ep.mu.RLock()

		// 填充 callbacks[] 切片，events[i] => callbacks[i]
		for i := 0; i < n; i++ {
			fd := int(events[i].Fd)

			// 检查 eventFd 上是否存在可读事件，如果存在，意味着监听结束了，直接 close 掉。
			if fd == ep.eventFd { // signal to close
				ep.mu.RUnlock()
				return
			}

			// 填充 callbacks[]
			callbacks[i] = ep.callbacks[fd]
		}
		ep.mu.RUnlock()

		// 循环调用 callbacks[] 里的回调函数。
		for i := 0; i < n; i++ {
			if cb := callbacks[i]; cb != nil {
				cb(EpollEvent(events[i].Events))
				callbacks[i] = nil
			}
		}

		// 扩容检查：如果当前 events 已经被触发事件填满，则可能是不够用了，进行双倍扩容。
		if n == len(events) && n*2 <= maxWaitEventsStop {
			events = make([]unix.EpollEvent, n*2)
			callbacks = make([]func(EpollEvent), 0, n*2)
		}
	}
}
