package collect

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"llamadash/internal/container"
)

func someList() []container.Entry {
	return []container.Entry{{Name: "web-1", State: "running"}, {Name: "llama", State: "exited", ExitCode: 128, HasExit: true}}
}

func dockerStub(calls *int32, ov container.Overview, err error) func(context.Context, int) (container.Overview, error) {
	return func(context.Context, int) (container.Overview, error) {
		atomic.AddInt32(calls, 1)
		return ov, err
	}
}

// The list comes from the same lookup as Container, and is there when no
// container publishes the port.
func TestContainersListed(t *testing.T) {
	var calls int32
	ov := container.Overview{List: someList(), LlamaErr: container.ErrNotFound}
	c := newC("http://127.0.0.1:8011", func(c *Config) { c.GPU, c.Docker = GPUAuto, dockerStub(&calls, ov, nil) })
	c.Poll(context.Background(), t0)
	waitContainerIdle(t, c)
	s := c.Poll(context.Background(), t0.Add(time.Second))
	if len(s.Containers) != 2 || s.Containers[1].ExitCode != 128 || !s.ContainersAt.Equal(t0) || s.Container != nil {
		t.Fatalf("%+v at %v, container %+v", s.Containers, s.ContainersAt, s.Container)
	}
	s.Containers[0].Name = "changed" // the snapshot owns its copy
	if s := c.Poll(context.Background(), t0.Add(2*time.Second)); s.Containers[0].Name != "web-1" {
		t.Fatal("snapshot shares the collector's list")
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("calls %d", calls)
	}
	// both at once
	ov.Llama, ov.LlamaErr = container.Info{Name: "llama"}, nil
	c = newC("http://127.0.0.1:8011", func(c *Config) { c.GPU, c.Docker = GPUAuto, dockerStub(&calls, ov, nil) })
	c.Poll(context.Background(), t0)
	waitContainerIdle(t, c)
	if s := c.Poll(context.Background(), t0.Add(time.Second)); len(s.Containers) != 2 || s.Container == nil || s.Container.Name != "llama" {
		t.Fatalf("%+v %+v", s.Containers, s.Container)
	}
}

func TestContainersOnlyWithLocalSources(t *testing.T) {
	var calls int32
	ov := container.Overview{List: someList()}
	for _, mut := range []func(*Config){
		func(c *Config) { c.URL, c.GPU = "http://192.0.2.10:8011", GPUAuto },
		func(c *Config) { c.GPU = GPUOff },
	} {
		c := newC("http://127.0.0.1:8011", func(c *Config) { c.Docker = dockerStub(&calls, ov, nil); mut(c) })
		if s := c.Poll(context.Background(), t0); s.Containers != nil || atomic.LoadInt32(&calls) != 0 {
			t.Fatalf("read without local sources: %+v", s.Containers)
		}
	}
}

func TestContainersErrors(t *testing.T) {
	var mode atomic.Int32 // 0 ok, 1 daemon down, 2 other error, 3 unavailable
	var calls int32
	c := newC("http://127.0.0.1:8011", func(c *Config) {
		c.GPU = GPUAuto
		c.Docker = func(context.Context, int) (container.Overview, error) {
			atomic.AddInt32(&calls, 1)
			switch mode.Load() {
			case 1:
				return container.Overview{}, container.ErrDaemonDown
			case 2:
				return container.Overview{}, errors.New("boom")
			case 3:
				return container.Overview{}, container.ErrUnavailable
			}
			return container.Overview{List: someList(), Llama: container.Info{Name: "llama"}}, nil
		}
	})
	poll := func(sec int) Snapshot {
		s := c.Poll(context.Background(), t0.Add(time.Duration(sec)*time.Second))
		waitContainerIdle(t, c)
		return s
	}
	poll(0)
	if s := poll(1); len(s.Containers) != 2 {
		t.Fatal("first list not shown")
	}
	mode.Store(2)
	poll(10)
	if s := poll(11); len(s.Containers) != 2 || !s.ContainersAt.Equal(t0) || s.Container == nil {
		t.Fatalf("other error dropped the list: %+v", s.Containers)
	}
	mode.Store(1)
	poll(20)
	if s := poll(21); s.Containers != nil || s.Container != nil {
		t.Fatalf("kept while the daemon is down: %+v", s.Containers)
	}
	mode.Store(0)
	poll(30)
	if s := poll(31); len(s.Containers) != 2 || !s.ContainersAt.Equal(t0.Add(30*time.Second)) {
		t.Fatalf("not back after the daemon: %+v", s.Containers)
	}
	mode.Store(3)
	poll(40)
	if s := poll(41); s.Containers != nil {
		t.Fatalf("kept when docker is unavailable: %+v", s.Containers)
	}
	n := atomic.LoadInt32(&calls)
	poll(60)
	if atomic.LoadInt32(&calls) != n {
		t.Fatal("unavailable docker is retried")
	}
}

func TestContainersHangExpires(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	var calls int32
	c := newC("http://127.0.0.1:8011", func(c *Config) {
		c.GPU = GPUAuto
		c.Docker = func(context.Context, int) (container.Overview, error) {
			if atomic.AddInt32(&calls, 1) > 1 {
				<-release
			}
			return container.Overview{List: someList(), LlamaErr: container.ErrNotFound}, nil
		}
	})
	c.Poll(context.Background(), t0)
	waitContainerIdle(t, c)
	c.Poll(context.Background(), t0.Add(10*time.Second)) // hangs
	c.Poll(context.Background(), t0.Add(11*time.Second)) // seen in flight
	if s := c.Poll(context.Background(), t0.Add(40*time.Second)); s.Containers == nil {
		t.Fatal("expired before 30s")
	}
	if s := c.Poll(context.Background(), t0.Add(41*time.Second)); s.Containers != nil {
		t.Fatalf("stuck list kept: %+v", s.Containers)
	}
}
