# Fix log — REVIEW_MYTHOS51_XHIGH

Source: `review/2026/10/REVIEW_MYTHOS51_XHIGH.md`; baseline `be04ae7`.
Entries follow the review's suggested fix order. Verification limitations are recorded explicitly.

- RM51X-069 — FIXED: retry temporary accept errors with WARN logging and 5 ms–1 s interruptible backoff, resetting after success; permanent errors and TLS connection handling retained. Files: `imap/imapsess/handshake.go`, `imap/imapsess/handshake_rm51x069_test.go`. Verification: `cd imap && go test -race ./... -run 'TestTLSHandshake|Test.*TLS|Test.*Handshake' -count=1` PASS (including injected EMFILE recovery and existing TLS handshake tests); real low-ulimit exhaustion check pending.
