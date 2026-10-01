package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/ptudor/epistula-mail/database/storage"
)

type jobTarget struct {
	MailboxID int64 `json:"mailbox_id"`
	FolderID  int64 `json:"folder_id"`
}

// Parse with the same driver as storage: keyword quoting, ports, service files,
// environment defaults and fallback hosts all participate. Never serialize a
// password, TLS private key, passfile or the original DSN.
func redactedDSN(dsn string) string {
	c, err := pgx.ParseConfig(dsn)
	if err != nil {
		return "(invalid PostgreSQL destination)"
	}
	return strings.Join(destinationEndpoints(c), ",") + "/" + c.Database
}

func destinationEndpoints(c *pgx.ConnConfig) []string {
	endpoints := []string{net.JoinHostPort(c.Host, strconv.Itoa(int(c.Port)))}
	for _, f := range c.Fallbacks {
		e := net.JoinHostPort(f.Host, strconv.Itoa(int(f.Port)))
		found := false
		for _, old := range endpoints {
			if old == e {
				found = true
				break
			}
		}
		if !found {
			endpoints = append(endpoints, e)
		}
	}
	return endpoints
}

// bindJob resolves both the effective connection and the actual destination
// tables/identities. Recreated accounts, folders, schemas or databases cannot
// inherit an old resume marker merely because their names match.
func bindJob(ctx context.Context, db *storage.DB, j *jobIdentity) error {
	c := db.Pool().Config().ConnConfig
	identity := struct {
		Endpoints      []string
		Database, Role string
		Tables         []int64
	}{Endpoints: destinationEndpoints(c)}
	if err := db.Pool().QueryRow(ctx, `SELECT current_database(),current_user,ARRAY['mailboxes'::regclass::oid::bigint,'folders'::regclass::oid::bigint,'messages'::regclass::oid::bigint]`).Scan(&identity.Database, &identity.Role, &identity.Tables); err != nil {
		return err
	}
	encoded, _ := json.Marshal(identity)
	sum := sha256.Sum256(encoded)
	j.DestinationKey = hex.EncodeToString(sum[:])
	j.Targets = make(map[string]jobTarget)
	rows, err := db.Pool().Query(ctx, `SELECT m.name,m.id,COALESCE(f.id,0) FROM mailboxes m LEFT JOIN folders f ON f.mailbox_id=m.id AND f.name=$1 WHERE $2='' OR m.name=$2 ORDER BY m.name`, j.Folder, j.Mailbox)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var target jobTarget
		if err := rows.Scan(&name, &target.MailboxID, &target.FolderID); err != nil {
			return err
		}
		j.Targets[name] = target
	}
	return rows.Err()
}

// The first successful ingest may create a previously absent folder. Carry
// that durable binding into the next checkpoint and failure manifest.
func (j *jobIdentity) bindCreatedFolder(name string, mailboxID, folderID int64) error {
	target, ok := j.Targets[name]
	if !ok || target.MailboxID != mailboxID || (target.FolderID != 0 && target.FolderID != folderID) {
		return fmt.Errorf("%w: destination identity changed for %s", errCheckpointMismatch, name)
	}
	target.FolderID = folderID
	j.Targets[name] = target
	return nil
}
