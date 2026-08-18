package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jhoelzel/simpleapp/pkg/router"
	"github.com/jhoelzel/simpleapp/pkg/version"
)

const (
	defaultHTTPPort = 8080
	demoUDPPort     = 5060
	demoTCPPort1    = 5060
	demoTCPPort2    = 5061
	maxTCPClients   = 128
)

var demoTCPClientSlots = make(chan struct{}, maxTCPClients)

func main() {
	log.Printf(
		"starting simpleapp release=%s commit=%s build_time=%s",
		version.Release, version.Commit, version.BuildTime,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	httpPort := configuredPort("PORT", defaultHTTPPort)
	srv := &http.Server{
		Addr:              net.JoinHostPort("", strconv.Itoa(httpPort)),
		Handler:           router.Get(version.Release),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	errCh := make(chan error, 4)
	go runHTTPServer(srv, errCh)
	go runTCPDemo(ctx, demoTCPPort1, errCh)
	go runTCPDemo(ctx, demoTCPPort2, errCh)
	go runUDPDemo(ctx, demoUDPPort, errCh)

	log.Printf("listeners ready: http=%d tcp=%d,%d udp=%d", httpPort, demoTCPPort1, demoTCPPort2, demoUDPPort)

	select {
	case <-ctx.Done():
		log.Print("shutdown signal received")
	case err := <-errCh:
		log.Printf("listener stopped unexpectedly: %v", err)
		stop()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP shutdown did not complete cleanly: %v", err)
	}
	log.Print("simpleapp stopped")
}

func configuredPort(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 || port == demoTCPPort1 || port == demoTCPPort2 {
		log.Printf("ignoring invalid %s=%q; using %d", name, value, fallback)
		return fallback
	}
	return port
}

func runHTTPServer(srv *http.Server, errCh chan<- error) {
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		errCh <- fmt.Errorf("HTTP %s: %w", srv.Addr, err)
	}
}

func runTCPDemo(ctx context.Context, port int, errCh chan<- error) {
	address := net.JoinHostPort("", strconv.Itoa(port))
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", address)
	if err != nil {
		errCh <- fmt.Errorf("TCP %s: %w", address, err)
		return
	}
	defer listener.Close()

	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	retryDelay := time.Duration(0)
	for {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			if ctx.Err() != nil || errors.Is(acceptErr, net.ErrClosed) {
				return
			}
			if retryDelay == 0 {
				retryDelay = 5 * time.Millisecond
			} else {
				retryDelay *= 2
				if retryDelay > time.Second {
					retryDelay = time.Second
				}
			}
			log.Printf("TCP %s accept error; retrying in %s: %v", address, retryDelay, acceptErr)
			if !waitForRetry(ctx, retryDelay) {
				return
			}
			continue
		}
		retryDelay = 0
		select {
		case demoTCPClientSlots <- struct{}{}:
			go func() {
				defer func() { <-demoTCPClientSlots }()
				handleTCPConnection(conn)
			}()
		default:
			_ = conn.Close()
		}
	}
}

func handleTCPConnection(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = conn.Write([]byte("simpleapp TCP echo ready\n"))

	buffer := make([]byte, 4096)
	for {
		n, err := conn.Read(buffer)
		if n > 0 {
			if _, writeErr := conn.Write(buffer[:n]); writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func runUDPDemo(ctx context.Context, port int, errCh chan<- error) {
	address := net.JoinHostPort("", strconv.Itoa(port))
	packetConn, err := (&net.ListenConfig{}).ListenPacket(ctx, "udp", address)
	if err != nil {
		errCh <- fmt.Errorf("UDP %s: %w", address, err)
		return
	}
	defer packetConn.Close()

	go func() {
		<-ctx.Done()
		_ = packetConn.Close()
	}()

	buffer := make([]byte, 4096)
	for {
		n, remote, readErr := packetConn.ReadFrom(buffer)
		if readErr != nil {
			if ctx.Err() != nil || errors.Is(readErr, net.ErrClosed) {
				return
			}
			log.Printf("UDP %s read error: %v", address, readErr)
			if !waitForRetry(ctx, 50*time.Millisecond) {
				return
			}
			continue
		}
		if _, writeErr := packetConn.WriteTo(buffer[:n], remote); writeErr != nil {
			log.Printf("UDP %s echo error: %v", address, writeErr)
		}
	}
}

func waitForRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
