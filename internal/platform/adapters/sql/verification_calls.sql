-- Platform module — VerificationCall queries. Per ADR 0059.

-- name: InsertVerificationCall :exec
INSERT INTO platform.verification_calls (
    id, contact_id, outcome_code, notes,
    callback_window_start_at, callback_window_end_at,
    logged_at, logged_by_membership_id
) VALUES (
    $1, $2, $3, $4,
    $5, $6,
    $7, $8
);

-- name: ListVerificationCallsByContact :many
SELECT id, contact_id, outcome_code, notes,
       callback_window_start_at, callback_window_end_at,
       logged_at, logged_by_membership_id
FROM   platform.verification_calls
WHERE  contact_id = $1
ORDER  BY logged_at DESC;
