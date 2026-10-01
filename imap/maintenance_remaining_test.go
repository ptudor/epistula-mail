package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/auth"
	"github.com/ptudor/epistula-mail/database/maintenance"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

func TestMaintenanceServerProcess(t *testing.T) {
	if cfg := os.Getenv("MAIL_TEST_MAINTENANCE_SERVER"); cfg != "" {
		os.Exit(runServe([]string{"-config", cfg}))
	}
}

func TestAuthenticatedServerBlocksOfflineMaintenance(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx := context.Background()
	hash, err := auth.HashPassword("test-password", auth.DefaultParams())
	if err != nil {
		t.Fatal(err)
	}
	var mailbox int64
	if err := db.Pool().QueryRow(ctx, `INSERT INTO mailboxes(name,password_hash) VALUES('barrier',$1) RETURNING id`, hash).Scan(&mailbox); err != nil {
		t.Fatal(err)
	}
	if _, err := db.LookupOrCreateFolder(ctx, mailbox, "INBOX"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	cfg := filepath.Join(t.TempDir(), "imap.toml")
	text := fmt.Sprintf("production=false\n[server]\nlisten_addr=%q\naccept_insecure_for_dev=true\ndrain_timeout_seconds=1\n[admin]\nlisten_addr='127.0.0.1:0'\n[postgres]\ndsn=%q\nmax_open_conns=2\nmax_idle_conns=0\nidle_conn_pool_size=0\n[storage]\nroot=%q\n", addr, dsn, root)
	if err := os.WriteFile(cfg, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMaintenanceServerProcess$")
	cmd.Env = append(os.Environ(), "MAIL_TEST_MAINTENANCE_SERVER="+cfg)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	var conn net.Conn
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		conn, err = net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal("server did not listen", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	reader := bufio.NewReader(conn)
	if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "* OK") {
		t.Fatal("greeting", line, err)
	}
	reply := func(tag string) {
		t.Helper()
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(line, tag+" ") {
				if !strings.HasPrefix(line, tag+" OK") {
					t.Fatal(line)
				}
				return
			}
		}
	}
	fmt.Fprint(conn, "a LOGIN barrier test-password\r\n")
	reply("a")
	// Hold an authenticated APPEND at its literal boundary. Maintenance must
	// refuse while the daemon can still create a blob under its cached tenant.
	raw := "Subject: barrier\r\n\r\nbody\r\n"
	fmt.Fprintf(conn, "b APPEND INBOX {%d}\r\n", len(raw))
	if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "+") {
		t.Fatal(line, err)
	}
	if lease, err := maintenance.Acquire(ctx, root, db.Pool(), true, false); err == nil {
		lease.Close()
		t.Fatal("maintenance entered during authenticated APPEND")
	}
	fmt.Fprint(conn, raw+"\r\n")
	reply("b")
	fmt.Fprint(conn, "c SELECT INBOX\r\n")
	reply("c")
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal("drain", err)
	}
	waited = true
	if _, err := reader.ReadString('\n'); err == nil { // BYE may precede EOF.
		if _, err = reader.ReadString('\n'); err == nil {
			t.Fatal("old session remains live")
		}
	}
	lease, err := maintenance.Acquire(ctx, root, db.Pool(), true, false)
	if err != nil {
		t.Fatal("process exit did not release barrier", err)
	}
	if _, err := db.Pool().Exec(ctx, `UPDATE mailboxes SET maintenance_at=now() WHERE id=$1`, mailbox); err != nil {
		t.Fatal(err)
	}
	lease.Close()
	cmd = exec.Command(os.Args[0], "-test.run=^TestMaintenanceServerProcess$")
	cmd.Env = append(os.Environ(), "MAIL_TEST_MAINTENANCE_SERVER="+cfg)
	if err := cmd.Run(); err == nil {
		t.Fatal("IMAP started while store is in maintenance")
	}
}
