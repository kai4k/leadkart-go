-- LeadKart Go — Phase 2 Slice A.2 — CRM CallLog callback window columns.
--
-- BRD §4.5 Callback Window — when a contact / lead is busy, the caller
-- logs the call WITH a callback Date + Window Start/End Time. The CRM
-- side persists those values on the call_log row + emits them on the
-- CallLogged integration event; the Reminder slice's CallLogged
-- subscriber consumes the START time to mint a callback reminder.
--
-- v0.2 stores three nullable timestamptz columns. The factory enforces
-- the all-or-nothing invariant (start + end together, both NULL when
-- the call did not request a callback). When all three are NULL the
-- subscriber does not mint a reminder.

-- +goose Up
-- +goose StatementBegin

ALTER TABLE crm.call_logs
    ADD COLUMN callback_window_start_at timestamptz NULL,
    ADD COLUMN callback_window_end_at   timestamptz NULL;

-- All-or-nothing: both NULL OR both set. Caller side decides whether to
-- supply a callback window; we don't allow start without end (or vice
-- versa) at the DB level.
ALTER TABLE crm.call_logs
    ADD CONSTRAINT chk_call_logs_callback_window
        CHECK (
            (callback_window_start_at IS NULL AND callback_window_end_at IS NULL) OR
            (callback_window_start_at IS NOT NULL AND callback_window_end_at IS NOT NULL
             AND callback_window_end_at >= callback_window_start_at)
        );

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE crm.call_logs DROP CONSTRAINT IF EXISTS chk_call_logs_callback_window;
ALTER TABLE crm.call_logs DROP COLUMN IF EXISTS callback_window_end_at;
ALTER TABLE crm.call_logs DROP COLUMN IF EXISTS callback_window_start_at;

-- +goose StatementEnd
