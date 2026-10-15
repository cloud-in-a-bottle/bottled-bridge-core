-- The message store. The bridge keeps its own bookkeeping (ID mappings, encryption keys, backfill queue) in a
-- separate database; this one holds the conversations and messages themselves, and is the only copy of them.

CREATE TABLE IF NOT EXISTS people (
    id INTEGER PRIMARY KEY,
    -- The network's user ID. 'bot' is the bridge itself, which authors notices and the occasional senderless event;
    -- 'name:<name>' is someone known only by name, from an imported export.
    remote_id TEXT NOT NULL UNIQUE,
    is_self INTEGER NOT NULL DEFAULT 0,
    name TEXT,
    avatar_sha256 TEXT,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS conversations (
    id INTEGER PRIMARY KEY,
    -- The bridge's room ID for this chat, derived from the network's chat ID.
    room_id TEXT NOT NULL UNIQUE,
    -- The network's chat ID (the bridge's portal ID).
    thread_key TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('dm', 'group', 'space')),
    other_person_id INTEGER REFERENCES people(id),
    parent_id INTEGER REFERENCES conversations(id),
    title TEXT,
    topic TEXT,
    avatar_sha256 TEXT,
    pinned INTEGER NOT NULL DEFAULT 0,
    archived INTEGER NOT NULL DEFAULT 0,
    muted_until INTEGER NOT NULL DEFAULT 0,
    marked_unread INTEGER NOT NULL DEFAULT 0,
    disappear_type TEXT,
    disappear_timer_ms INTEGER NOT NULL DEFAULT 0,
    last_read_at INTEGER NOT NULL DEFAULT 0,
    last_message_at INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    -- Set when the chat is deleted or replaced on the network. Its history is kept, but it's hidden from the list.
    deleted_at INTEGER
);

CREATE TABLE IF NOT EXISTS conversation_members (
    conversation_id INTEGER NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    person_id INTEGER NOT NULL REFERENCES people(id),
    membership TEXT NOT NULL,
    PRIMARY KEY (conversation_id, person_id)
);

-- Room state as the bridge set it (names, avatars, members, power levels, ...), keyed like Matrix state. The
-- columns above are derived from it; this is what the bridge reads back.
CREATE TABLE IF NOT EXISTS room_state (
    room_id TEXT NOT NULL,
    type TEXT NOT NULL,
    state_key TEXT NOT NULL,
    sender TEXT NOT NULL,
    content TEXT NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (room_id, type, state_key)
);

CREATE TABLE IF NOT EXISTS messages (
    id INTEGER PRIMARY KEY,
    conversation_id INTEGER NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    author_id INTEGER NOT NULL REFERENCES people(id),
    -- The network's message ID. NULL for a message sent from here until the network confirms it.
    remote_id TEXT,
    sent_at INTEGER NOT NULL,
    received_at INTEGER NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('message', 'update')),
    msgtype TEXT,
    body TEXT,
    formatted_body TEXT,
    reply_to_event TEXT,
    thread_root_event TEXT,
    link_previews TEXT,
    edited_at INTEGER,
    deleted_at INTEGER,
    expires_at INTEGER,
    -- Only for messages sent from this account.
    status TEXT CHECK (status IN ('pending', 'sent', 'delivered', 'failed')),
    error TEXT,
    source TEXT NOT NULL CHECK (source IN ('live', 'backfill', 'sent', 'import')),
    UNIQUE (conversation_id, remote_id)
);
CREATE INDEX IF NOT EXISTS messages_conversation_sent ON messages (conversation_id, sent_at);
CREATE INDEX IF NOT EXISTS messages_expires ON messages (expires_at) WHERE expires_at IS NOT NULL;

-- A message arrives as one or more parts (eg a caption and two photos), each with its own event ID. Replies,
-- reactions, edits and deletions refer to parts by event ID.
CREATE TABLE IF NOT EXISTS message_parts (
    event_id TEXT PRIMARY KEY,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    part_id TEXT NOT NULL,
    attachment_id INTEGER REFERENCES attachments(id) ON DELETE SET NULL
);
CREATE INDEX IF NOT EXISTS message_parts_message ON message_parts (message_id);

-- Earlier versions of edited messages. edited_at is when that version was replaced.
CREATE TABLE IF NOT EXISTS message_edits (
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    revision INTEGER NOT NULL,
    body TEXT,
    edited_at INTEGER NOT NULL,
    PRIMARY KEY (message_id, revision)
);

CREATE TABLE IF NOT EXISTS reactions (
    event_id TEXT NOT NULL UNIQUE,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    reactor_id INTEGER NOT NULL REFERENCES people(id),
    emoji TEXT NOT NULL,
    sent_at INTEGER NOT NULL,
    PRIMARY KEY (message_id, reactor_id, emoji)
);

-- How far each member has read, as a message timestamp.
CREATE TABLE IF NOT EXISTS read_receipts (
    conversation_id INTEGER NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    person_id INTEGER NOT NULL REFERENCES people(id),
    read_up_to INTEGER NOT NULL,
    at INTEGER NOT NULL,
    PRIMARY KEY (conversation_id, person_id)
);

CREATE TABLE IF NOT EXISTS attachments (
    id INTEGER PRIMARY KEY,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL,
    msgtype TEXT NOT NULL,
    content_type TEXT,
    filename TEXT,
    size INTEGER,
    width INTEGER,
    height INTEGER,
    duration_ms INTEGER,
    caption TEXT,
    blurhash TEXT,
    is_voice_note INTEGER NOT NULL DEFAULT 0,
    is_sticker INTEGER NOT NULL DEFAULT 0,
    file_sha256 TEXT,
    status TEXT NOT NULL CHECK (status IN ('present', 'missing', 'purged')),
    UNIQUE (message_id, ordinal)
);
CREATE INDEX IF NOT EXISTS attachments_sha ON attachments (file_sha256);

-- Every timeline event as the bridge delivered it, for reprocessing if the conversion above turns out lossy.
CREATE TABLE IF NOT EXISTS raw_events (
    id INTEGER PRIMARY KEY,
    room_id TEXT NOT NULL,
    event_id TEXT NOT NULL,
    type TEXT NOT NULL,
    sender TEXT NOT NULL,
    ts INTEGER NOT NULL,
    content TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS raw_events_room ON raw_events (room_id);

CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5 (body, content='messages', content_rowid='id');
CREATE TRIGGER IF NOT EXISTS messages_fts_insert AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts (rowid, body) VALUES (new.id, new.body);
END;
CREATE TRIGGER IF NOT EXISTS messages_fts_delete AFTER DELETE ON messages BEGIN
    INSERT INTO messages_fts (messages_fts, rowid, body) VALUES ('delete', old.id, old.body);
END;
CREATE TRIGGER IF NOT EXISTS messages_fts_update AFTER UPDATE OF body ON messages BEGIN
    INSERT INTO messages_fts (messages_fts, rowid, body) VALUES ('delete', old.id, old.body);
    INSERT INTO messages_fts (rowid, body) VALUES (new.id, new.body);
END;

CREATE TABLE IF NOT EXISTS kv (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
