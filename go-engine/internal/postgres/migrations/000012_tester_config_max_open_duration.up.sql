-- Panel-editable max_open_duration (2026-08-30 follow-up): stored as the same Go duration string
-- format as config.yaml's tester.max_open_duration (e.g. "6h"), parsed at cmd/strategy-tester
-- startup the same way the YAML value is -- avoids a second representation (seconds, an interval
-- type) that would need its own parsing/formatting path on both the Go and panel sides.
ALTER TABLE tester_config
    ADD COLUMN IF NOT EXISTS max_open_duration TEXT;
