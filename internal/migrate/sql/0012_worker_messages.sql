CREATE TABLE worker_messages (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
 sender_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
 recipient_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
 message_id TEXT NOT NULL CHECK(length(message_id) BETWEEN 1 AND 128),
 body TEXT NOT NULL CHECK(length(CAST(body AS BLOB)) BETWEEN 1 AND 8192),
 created_at TEXT NOT NULL,
 received_at TEXT NOT NULL DEFAULT '',
 UNIQUE(session_id,sender_id,message_id)
) STRICT;
CREATE INDEX worker_messages_inbox ON worker_messages(session_id,recipient_id,seq);
CREATE INDEX worker_messages_sender ON worker_messages(sender_id,seq);
