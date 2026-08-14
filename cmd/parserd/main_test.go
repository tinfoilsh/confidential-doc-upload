package main

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDecodeFilenameAcceptsOnlyBasename(t *testing.T) {
	const safeName = "0123456789abcdef0123456789abcdef.pdf"
	valid := base64.RawURLEncoding.EncodeToString([]byte(safeName))
	if got, err := decodeFilename(valid); err != nil || got != safeName {
		t.Fatalf("decodeFilename(valid) = %q, %v", got, err)
	}
	for _, value := range []string{
		"../secret", "/secret", "", "random.pdf",
		"0123456789abcdef0123456789abcdeg.pdf",
		"0123456789abcdef0123456789abcdef.PDF",
		"0123456789abcdef0123456789abcdef.bad-name",
		string(make([]byte, 256)),
	} {
		encoded := base64.RawURLEncoding.EncodeToString([]byte(value))
		if _, err := decodeFilename(encoded); err == nil {
			t.Fatalf("decodeFilename(%q) unexpectedly succeeded", value)
		}
	}
}

func TestParserSlotHonorsCancellation(t *testing.T) {
	slots := make(chan struct{}, 1)
	slots <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := acquireParserSlot(ctx, slots, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("acquireParserSlot() = %v, want context.Canceled", err)
	}
}

func TestParserSlotRejectsBoundedQueue(t *testing.T) {
	slots := make(chan struct{}, 1)
	slots <- struct{}{}
	if err := acquireParserSlot(context.Background(), slots, time.Millisecond); !errors.Is(err, errParserBusy) {
		t.Fatalf("acquireParserSlot() = %v, want errParserBusy", err)
	}
}

func TestPeerUIDUsesKernelCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peer.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		connection, _ := listener.Accept()
		accepted <- connection
	}()
	client, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	select {
	case connection := <-accepted:
		if connection == nil {
			t.Fatal("listener did not accept connection")
		}
		defer connection.Close()
		uid, err := peerUID(connection)
		if err != nil {
			t.Fatal(err)
		}
		if uid != uint32(os.Getuid()) {
			t.Fatalf("peer UID = %d, want %d", uid, os.Getuid())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out accepting Unix connection")
	}
}

func TestPrepareSocketRefusesNonSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "parser.sock")
	if err := os.WriteFile(path, []byte("do not delete"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := prepareSocket(path); err == nil {
		t.Fatal("prepareSocket() accepted a non-socket path")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("non-socket path was removed: %v", err)
	}
}

func TestSocketDirectoryLockRejectsSecondBroker(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "parser.sock")
	first, err := lockSocketDirectory(socketPath)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	defer first.Close()

	second, err := lockSocketDirectory(socketPath)
	if second != nil {
		second.Close()
		t.Fatal("second broker unexpectedly acquired the lock")
	}
	if err == nil {
		t.Fatal("second broker lock unexpectedly succeeded")
	}
}
