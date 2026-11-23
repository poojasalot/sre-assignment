-- Demo schema: idempotent seed operations make local recreation predictable.
BEGIN;

CREATE TABLE IF NOT EXISTS users (
    id VARCHAR(36) PRIMARY KEY,
    email VARCHAR(255) UNIQUE NOT NULL
);

CREATE TABLE IF NOT EXISTS groups (
    id VARCHAR(36) PRIMARY KEY,
    name VARCHAR(100) NOT NULL UNIQUE
);

CREATE TABLE IF NOT EXISTS memberships (
    user_id VARCHAR(36) NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    group_id VARCHAR(36) NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, group_id)
);

CREATE TABLE IF NOT EXISTS group_permissions (
    group_id VARCHAR(36) NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    permission VARCHAR(100) NOT NULL,
    PRIMARY KEY (group_id, permission)
);

-- Reverse lookup indexes support joins by group_id and permission grants.
CREATE INDEX IF NOT EXISTS idx_memberships_group_id ON memberships(group_id);
CREATE INDEX IF NOT EXISTS idx_group_permissions_permission ON group_permissions(permission);

INSERT INTO groups (id, name) VALUES
    ('grp-1', 'researchers'),
    ('grp-2', 'admins')
ON CONFLICT (id) DO NOTHING;

INSERT INTO group_permissions (group_id, permission) VALUES
    ('grp-1', 'read:data'), ('grp-1', 'write:data'),
    ('grp-2', 'read:data'), ('grp-2', 'admin:users')
ON CONFLICT (group_id, permission) DO NOTHING;

INSERT INTO users (id, email) VALUES
    ('user-1', 'alice@example.com'),
    ('user-2', 'bob@example.com')
ON CONFLICT (id) DO NOTHING;

INSERT INTO memberships (user_id, group_id) VALUES
    ('user-1', 'grp-1'),
    ('user-2', 'grp-2')
ON CONFLICT (user_id, group_id) DO NOTHING;

COMMIT;
