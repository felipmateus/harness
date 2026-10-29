-- +goose Up
CREATE TABLE events (
  pos    INTEGER PRIMARY KEY,                                   -- rowid: global commit order
  stream TEXT    NOT NULL CHECK (length(stream) BETWEEN 1 AND 128),
  seq    INTEGER NOT NULL CHECK (seq > 0),
  id     TEXT    NOT NULL UNIQUE,
  type   TEXT    NOT NULL CHECK (length(type) BETWEEN 1 AND 128),
  time   TEXT    NOT NULL CHECK (length(time) = 24),            -- 2006-01-02T15:04:05.000Z
  data   BLOB    NOT NULL CHECK (length(data) BETWEEN 2 AND 1048576),
  UNIQUE (stream, seq)
);
-- +goose Down
DROP TABLE events;
