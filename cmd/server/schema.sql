-- Chat History Database Schema
-- Compatible with SQLite and PostgreSQL

-- Conversations table stores conversation metadata
CREATE TABLE IF NOT EXISTS conversations (
    id TEXT PRIMARY KEY,
    phone_number TEXT,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Messages table stores individual messages
CREATE TABLE IF NOT EXISTS messages (
    id SERIAL PRIMARY KEY,  -- Use INTEGER PRIMARY KEY AUTOINCREMENT for SQLite
    conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    role TEXT NOT NULL CHECK(role IN ('user', 'assistant')),
    content TEXT NOT NULL,
    phone_number TEXT,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Index for fast conversation lookups
CREATE INDEX IF NOT EXISTS idx_messages_conversation ON messages(conversation_id, created_at);

-- Index for time-based queries
CREATE INDEX IF NOT EXISTS idx_messages_created_at ON messages(created_at);

-- Index for phone number lookups
CREATE INDEX IF NOT EXISTS idx_conversations_phone ON conversations(phone_number);

-- Trigger to update the updated_at timestamp on conversations (SQLite syntax)
-- For PostgreSQL, use:
-- CREATE OR REPLACE FUNCTION update_updated_at_column()
-- RETURNS TRIGGER AS $$
-- BEGIN
--     NEW.updated_at = CURRENT_TIMESTAMP;
--     RETURN NEW;
-- END;
-- $$ language 'plpgsql';
--
-- CREATE TRIGGER update_conversations_updated_at
--     BEFORE UPDATE ON conversations
--     FOR EACH ROW
--     EXECUTE FUNCTION update_updated_at_column();
