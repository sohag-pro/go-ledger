-- +goose Up
-- +goose StatementBegin

-- Add an optional free-text narration to each posting (for example
-- "dinner repayment"). Bounded so it cannot grow unboundedly; the domain
-- enforces the same limit (MaxPostingDescriptionLen).
ALTER TABLE postings
    ADD COLUMN description text NOT NULL DEFAULT ''
    CHECK (char_length(description) <= 256);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE postings DROP COLUMN description;
-- +goose StatementEnd
