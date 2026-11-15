//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

const (
	instanceShow byte = iota
	instanceElevate
	instanceBackground
)

type desktopInstance struct {
	listener net.Listener
	show     chan struct{}
	quit     chan struct{}
	errors   chan error
	done     chan struct{}
}

func openDesktopInstance(background bool) (*desktopInstance, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("identify desktop user: %w", err)
	}
	var session uint32
	if err := windows.ProcessIdToSessionId(uint32(os.Getpid()), &session); err != nil {
		return nil, err
	}
	name := fmt.Sprintf(`\\.\pipe\UCD.Desktop.%s.%d`, user.User.Sid.String(), session)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return claimDesktopInstance(ctx, name, user.User.Sid.String(), token.IsElevated(), background)
}

// The exclusive named-pipe listener is also the instance lock. Its name is
// independent of executable path/version, and it is released on process exit.
// The medium integrity label permits normal/elevated copies of this user to
// activate or replace their own GUI. This pipe cannot perform engine operations.
func claimDesktopInstance(ctx context.Context, name, sid string, elevated, background bool) (*desktopInstance, error) {
	security := "D:P(A;;GA;;;" + sid + ")(A;;GA;;;SY)S:(ML;;NW;;;ME)"
	takingOver := false
	for {
		listener, listenErr := winio.ListenPipe(name, &winio.PipeConfig{SecurityDescriptor: security})
		if listenErr == nil {
			instance := &desktopInstance{listener: listener, show: make(chan struct{}, 1), quit: make(chan struct{}, 1), errors: make(chan error, 1), done: make(chan struct{})}
			go instance.serve(elevated)
			return instance, nil
		}
		if takingOver {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("wait for desktop administrator handoff: %w", errors.Join(ctx.Err(), listenErr))
			case <-time.After(100 * time.Millisecond):
				continue
			}
		}
		connection, err := winio.DialPipeContext(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("contact existing desktop: %w", errors.Join(listenErr, err))
		}
		deadline, _ := ctx.Deadline()
		_ = connection.SetDeadline(deadline)
		command := instanceShow
		if background {
			command = instanceBackground
		} else if elevated {
			command = instanceElevate
		}
		_, err = connection.Write([]byte{command})
		var reply [1]byte
		if err == nil {
			_, err = io.ReadFull(connection, reply[:])
		}
		_ = connection.Close()
		if err != nil {
			return nil, fmt.Errorf("activate existing desktop: %w", err)
		}
		if reply[0] == instanceShow {
			return nil, nil
		}
		if reply[0] != instanceElevate {
			return nil, errors.New("existing desktop rejected activation")
		}
		// The normal GUI acknowledged an administrator takeover. Wait for its
		// listener to close before creating the elevated window and tray icon.
		takingOver = true
	}
}

func (instance *desktopInstance) serve(elevated bool) {
	defer close(instance.done)
	for {
		connection, err := instance.listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				instance.errors <- fmt.Errorf("desktop activation listener: %w", err)
			}
			return
		}
		_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
		var command [1]byte
		if _, err := io.ReadFull(connection, command[:]); err == nil {
			reply := instanceShow
			switch command[0] {
			case instanceElevate:
				if !elevated {
					reply = instanceElevate
					select {
					case instance.quit <- struct{}{}:
					default:
					}
				} else {
					select {
					case instance.show <- struct{}{}:
					default:
					}
				}
			case instanceShow:
				select {
				case instance.show <- struct{}{}:
				default:
				}
			case instanceBackground:
			default:
				reply = 255
			}
			_, _ = connection.Write([]byte{reply})
		}
		_ = connection.Close()
	}
}

func (instance *desktopInstance) close() {
	_ = instance.listener.Close()
	<-instance.done
}
