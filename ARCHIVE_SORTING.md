# Archive sorting

**Archive** is the everyday "I'm done with this" action, and archiving files the
message for you. With `delete_archives` on, **Delete** archives too, as it does
in Gmail. Only **moving a message to Trash** destroys it. INBOX can stay at zero
while the history is kept forever, filed by category, and only mail you
deliberately throw away is destroyed.

An IMAP server never sees a key press. It sees one of three things, and each
has its own meaning:

| You do | Your client sends | The server does |
|---|---|---|
| **Archive** (button, swipe, ⌃⌘A) | MOVE to the folder marked `\Archive` | Files it into `Archive/<Category>` once it is classified |
| **Delete** (Delete key, trash button, swipe) *with the client set not to use Trash* | mark `\Deleted`, then EXPUNGE | With `delete_archives`: archives it, exactly as Archive does |
| **Move to Trash** (drag there, or *Message → Move To → Trash*) | MOVE to the folder marked `\Trash` | Destroys it after `trash_retention` (24 h), everywhere in the mailbox |

Gmail works the same way. Its IMAP server turns a client's delete (mark and
expunge) into archive, and Gmail users switch off their client's "move deleted
messages to Trash". No key has to be remapped.

## How it fits together

```
 new mail ──► INBOX                                  epistula-llm-worker
                │                                      reads each message through epistula-api,
                │   classified in the background ◄──── picks ONE key from the mailbox's
                │                                      approved list, PUTs it back
      Archive   ▼                                      (write_classification only)
 ─────────► "Archive" (\Archive) ──sorter (epistula-imap)──► Archive/Finance/Banking
                                                             Archive/Travel …
      Delete
 ─────────► "Deleted Messages" (\Trash) ──purge (epistula-imap, after 24 h)──► gone
                                           rows now; files at the next `gc` sweep
```

| Piece | Where | What it does |
|---|---|---|
| Category list | epistula-database, `archive_categories` (migration 020) | Per mailbox: a key the classifier may choose (up to three levels), the folder it files into, a description, and whether it is annual (filed by year). Loaded with `admin archive-category-import`. Every folder must be inside the mailbox's `\Archive` folder. |
| Classifier | epistula-llm-worker `[classify]` | Adds the archive category to the same model request as the annotation, so a backfill reads each message once. Writes `PUT /v1/messages/{id}/classification`. |
| Classification | epistula-api, `message_classifications` | Stores the choice after checking it against the mailbox's *active* list. Separate from annotations and written under its own permission, so re-summarizing, or epistula-mcp's `annotate` tool, can never re-file mail. |
| Live sorter | epistula-imap `[archive] sort_enabled` | Every minute: messages in `\Archive` that have settled (5 min, so a client's Undo still works), are classified at ≥ `min_confidence`, and are not marked `\Deleted` move to their category folder. Everything else stays in `\Archive`, which is the unsorted pile. |
| Trash purge | epistula-imap `[archive] purge_enabled` | Messages in `\Trash` longer than `trash_retention` are destroyed, with every other copy of the same content in the mailbox (`purge_all_copies`). |
| Reorganization | `epistula-database admin archive-plan / archive-apply / archive-undo` | One-time refiling of an imported archive into the category tree, in journaled batches that can be undone. |

Every server-side move is an in-place rewrite (`storage.MoveMessages`). The
message keeps its id, so its attachments, annotations and classification stay
attached, and Postgres keeps the out-of-line body values instead of copying
them. A client sees exactly an IMAP MOVE: new UIDs in the destination, EXPUNGE
in the source. Every move is journaled in `archive_moves`.

## Setting up a mailbox

1. **Schema.** `epistula-database migrate` applies migration 020, which also grants
   the new tables to the existing roles that already hold the API and IMAP privileges.
2. **Folder roles.** The mailbox needs an `\Archive` and a `\Trash` folder.
   Give existing folders the roles, or create them:

   ```sh
   epistula-database admin folder-set-special-use -mailbox jdoe -folder Archive -use Archive
   epistula-database admin folder-set-special-use -mailbox jdoe -folder "Deleted Messages" -use Trash
   epistula-database admin folder-create -mailbox asmith -folder Archive -use Archive
   epistula-database admin folder-create -mailbox asmith -folder "Deleted Messages" -use Trash
   ```

3. **Categories.** Write the list and load it (`-dry-run` first):

   ```toml
   [[category]]
   key         = "finance/banking/acme-bank"            # lower-case [a-z0-9-], up to three levels
   folder      = "Archive/Finance/Banking/Acme Bank"    # inside the \Archive folder, any depth
   description = "Acme Bank statements, card alerts, transfers"  # short: it is in every prompt
   annual      = true                                   # file into .../Acme Bank/2025

   [[category]]
   key    = "finance/banking/other"
   folder = "Archive/Finance/Banking/Other"
   annual = true
   ```

   **Annual** categories file into a year folder below their own. The year
   comes from the message's own date (the sender's calendar date, else the
   arrival date), never from the model. A message whose dates give no
   plausible year (before 1971, or after next year) files into the category
   folder itself. **Other** keys at each level give a weak match somewhere to
   land one level up rather than in a wrong leaf. The prompt tells the model
   to choose the `…/other` key under the closest shared parent when it is
   unsure between specific keys. The whole list goes into every classification
   request, in the system prompt ahead of the message, where the inference
   server's prefix cache can reuse it. Keep descriptions short.

   ```sh
   epistula-database admin archive-category-import -mailbox jdoe -file jdoe-categories.toml -dry-run
   epistula-database admin archive-category-import -mailbox jdoe -file jdoe-categories.toml
   ```

   Importing again replaces the list: new keys are added, changed ones
   updated, and keys no longer listed are retired. A retired key files
   nothing, and messages classified under it count as unclassified, so the
   worker classifies them again against the current list.
4. **Classifier.** Mint a token that can also write classifications, put it in
   the worker config, and set `[classify] enabled = true`:

   ```sh
   epistula-database admin api-token-add -name llm-worker-classify -mailboxes "*" \
       -permission read_content -permission write_annotation -permission write_classification
   ```

   The next pass annotates and classifies unannotated mail in one request each,
   then classifies (only) what was annotated earlier.
5. **Reorganize the history** (below), once the backfill has classified it.
6. **Turn on the sorter and delete-archives**, then the purge:
   `[archive] sort_enabled = true` and `delete_archives = true` in
   epistula-imap's config, restart it, and set up the mail clients (below).
   Enable `purge_enabled` only after `\Trash` holds nothing you want to keep
   (`archive-apply` drains it) and `gc` runs on a schedule.

## Reorganizing an imported archive

An import can leave one role scattered over several folders: "Sent",
"Sent/2005/06-Jun", "Sent/Old" beside the `\Sent` folder clients use.
Combine them first. The move is journaled, so `archive-undo` reverses it. A
message whose exact content is already in the destination is left behind, or
dropped with `-drop-duplicates`, so the merged folder holds it once:

```sh
epistula-database admin folder-merge -mailbox jdoe -from Sent -subtree -to "Sent Messages" -drop-duplicates -dry-run
epistula-database admin folder-merge -mailbox jdoe -from Sent -subtree -to "Sent Messages" -drop-duplicates -yes
```

A merge also records a folder redirect (migration 021), so a later Maildir
sync of the legacy folders lands in the merged folder. `import` follows a
redirect only when its target folder does not exist: once `folder-prune-empty`
has removed "Sent/2005/06-Jun", `import -folder Sent/2005/06-Jun` writes into
"Sent Messages" and recognises the messages the merge moved there, where it
would otherwise re-create the folder and store them a second time (import
deduplicates within its target folder). With `-subtree` the redirect also
covers folders below the source that did not exist at merge time. To import
into a merged folder's old name after all, create it first with
`admin folder-create`. `archive-undo` of the merge batch removes the redirect.
Merges made before migration 021 were recovered from the journal as
exact-name redirects, one per source folder that had a message moved.

Then:

```sh
epistula-database admin archive-plan  -mailbox jdoe -rules jdoe-rules.toml -moves-out /tmp/plan.tsv
epistula-database admin archive-apply -mailbox jdoe -rules jdoe-rules.toml -limit 500 -yes   # a trial
epistula-database admin archive-apply -mailbox jdoe -rules jdoe-rules.toml -yes
epistula-database admin folder-prune-empty -mailbox jdoe -dry-run
epistula-database admin folder-prune-empty -mailbox jdoe -yes
# and, if needed:
epistula-database admin archive-batches -mailbox jdoe
epistula-database admin archive-undo    -mailbox jdoe -batch reorg-20260926T031500Z -yes
```

`archive-plan` writes nothing. For each source folder it prints where its
messages would go and why the rest stay. `archive-apply` rebuilds the same plan
and carries it out 500 messages per transaction, each one journaled. It is
safe to interrupt and run again, because a message already in its destination
is not planned twice. Do it **before** IMAP clients have cached the archive.
Every moved message gets a new UID, and a client that already synchronised it
downloads it again.

A message's fate, in order:

| Case | Result |
|---|---|
| In a `keep` folder, or a `\Sent`, `\Drafts` or `\Junk` folder, or below one | stays (`keep`) |
| Already in a category folder, or a year folder of an annual one | stays (`filed`) |
| Marked `\Deleted` by a client | stays: moving it would carry the mark |
| INBOX, newer than `inbox_keep_days` | stays (`inbox-recent`) |
| Its folder has a `[folders]` rule | goes to that category (`folder-rule`) |
| Classified into an active category at ≥ `min_confidence` | goes to the category (`classified`) |
| Otherwise, in INBOX, `\Trash` or a `drain` folder | goes to the `\Archive` folder (`drain`) |
| Otherwise | stays (`unclassified` / `low-confidence`) |

`\Trash` is always drained. Mail there got there through the old "normal
delete", which is Archive now, and the purge would destroy it.

Rules file (every key optional):

```toml
min_confidence  = 0.6        # default 0.6
inbox_keep_days = 30         # default 30
keep  = ["Taxes", "Family"]  # a name covers that folder and everything below it
drain = ["webmail-archive"]  # empty these completely: unfiled mail goes to \Archive
[folders]                    # whole folders to one category, no model judgement
"Receipts/Acme Shop" = "shopping"
```

## Growing the category list

The categories only ever change when you change them. What the tooling does
is tell you where they are missing. It looks at the `…/other` buckets and at
everything classified below `min_confidence`, finds the senders (by
registrable domain, so `alerts.initech.example` is `initech.example`) and the
themes (by tag) that recur there, and proposes a sibling category for each:

```sh
epistula-database admin archive-category-suggest -mailbox jdoe                 # a report
epistula-database admin archive-category-suggest -mailbox jdoe -format toml    # commented [[category]] stanzas
```

A cluster is reported at 20 messages or 5% of its bucket (`-min`,
`-share`). A tag that more than 3% of the mailbox's annotations carry
("newsletter", "promotional") says what kind of mail it is, not what it is
about, so it is never a theme (`-max-tag-share`). Nor is a tag that is one
sender under another name (the tag "tidepool" is tidepool.example). Each
proposal is named beside its bucket's key (`shopping/stores/other` +
`acmeshop.example` → `shopping/stores/acmeshop`), copies the bucket's
`annual`, and is flagged if its key or folder already exists. Subjects in
the report are sender-controlled text; the report strips their control
characters. Nothing it prints is active: add what you want to the category
file, give it a description, and re-import.

Then have the messages that fell through look again:

```sh
epistula-database admin archive-reclassify -mailbox jdoe -key shopping/stores/other -refile -dry-run
epistula-database admin archive-reclassify -mailbox jdoe -key shopping/stores/other -refile -yes
```

This clears that key's classifications, so the worker's classification pass
classifies those messages again against the current list. That happens once
its running pass completes. With `-refile`, the messages the sorter or a
reorganization filed under the key, and which are still there, go back to the
`\Archive` folder (journaled: `archive-undo` puts them back), so the sorter
files them by their new classification. Anything you filed or moved yourself
stays where it is.

The model never adds a category. A list that mail could extend would let
anyone who can email you decide where your mail goes.

## Setting up mail clients

Make Delete archive, and keep Trash for the rare real deletion:

- **Mac Mail:** *Settings → Accounts → (account) → Mailbox Behaviors* —
  **uncheck "Move deleted messages to the Trash mailbox"**. The Delete key, the
  trash button and swipe then mark and expunge, and the server archives
  (`delete_archives`). To destroy something, drag it to Trash or use *Message →
  Move To → Trash*. The Archive button (⌃⌘A) archives too. If Mail does not
  pick up the server's folders, select them and use *Mailbox → Use This Mailbox
  For → Archive* (and *→ Trash*).
- **iPhone / iPad Mail:** in the account's *Advanced* settings, set *Move
  Discarded Messages Into* to **Archive Mailbox**. The trash button and swipe
  then archive. To destroy, use the message's *Move* menu and choose Trash.
- **Thunderbird:** *Server Settings → When I delete a message: Just mark it as
  deleted* (the server archives it), or use Archive (the A key) with the
  archive option set to a single folder, not yearly or monthly subfolders.

What the server does with a delete, in detail (`delete_archives`):

- In `\Trash`, `\Drafts` and `\Junk`, EXPUNGE destroys as always. Trash is the
  deliberate way to delete, a draft is a working copy, and junk is junk.
- Anywhere else, the **last copy** of a message moves to the `\Archive`
  folder, loses its `\Deleted` mark and is sorted. Inside the archive it stays
  where it is, because it is already archived.
- A message with another copy elsewhere in the mailbox is removed as before.
  That is a client's copy-then-delete move, and the content survives in the
  copy.
- A message left marked `\Deleted` in INBOX without an expunge is archived
  after `settle_delay`, so a client that only marks on Delete still empties
  INBOX. Undo within that window keeps it.
- A mailbox with no `\Archive` folder keeps plain IMAP behaviour.

## What "deleted" means

When a message is purged, its row goes, and with it (by cascade) its
attachment rows, annotations, classification and journal entries. With
`purge_all_copies`, so does every other message in the mailbox with the same
raw content. The raw and attachment files leave disk at the next `gc` cycle:

```sh
epistula-database gc -phase mark    # at least twice, some hours apart
epistula-database gc -phase sweep
```

That is deliberate. A file must be seen unreferenced by two mark passes and be
older than the grace period (24 h) before sweep removes it. **Run gc from
cron.** Without it, a destroyed message's files stay on disk.

Not reached by a purge: filesystem snapshots and backups keep the data until
they rotate out. `delivery_log` keeps the envelope line (from, to, size, a
hash prefix, no content) with its message link set to NULL.

## Security

This feature lets a model's output move mail. Before it, the worst a hostile
email could do through the LLM pipeline was produce a wrong annotation. The limits:

- **The model only chooses from your list.** The request constrains the answer
  to the mailbox's active keys. epistula-api refuses any key that is not active
  for the message's own mailbox. The key → folder mapping is operator data.
- **Only inside the archive.** The sorter files only into folders strictly
  inside `\Archive` that carry no special-use role, whatever the table says,
  and the model never names a folder or a year: those come from the operator's
  list and the message's own date.
  It never files INBOX mail on its own: it acts only on what the owner archived.
- **It never deletes.** Only the owner's own move to Trash leads to
  destruction. No model output can.
- **Separate credential.** `write_classification` is its own permission.
  epistula-mcp's interactive `annotate` tool, the one exposed to prompt injection
  through conversation, cannot touch filing.
- **Undoable.** Every move is journaled. `archive-undo` reverses a batch.

The worst case is a message filing itself in the wrong archive folder.
