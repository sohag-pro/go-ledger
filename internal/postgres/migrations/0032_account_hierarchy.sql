-- +goose Up
-- +goose StatementBegin

-- ADR-023: account hierarchy. A nullable self-referential parent_id makes a
-- chart of accounts a tree. Same-tenant parentage is enforced by a composite
-- foreign key reusing accounts' UNIQUE (tenant_id, id), the same guarantee
-- postings rely on, so a cross-tenant parent cannot be inserted.
ALTER TABLE accounts ADD COLUMN parent_id uuid;

ALTER TABLE accounts
  ADD CONSTRAINT accounts_parent_fk
  FOREIGN KEY (tenant_id, parent_id) REFERENCES accounts (tenant_id, id);

CREATE INDEX accounts_parent_idx ON accounts (tenant_id, parent_id);

-- Reject a self-parent, a cycle (walking up from the proposed parent and
-- reaching the row itself), and a child whose currency differs from its
-- parent's, on insert or whenever parent_id changes. The database is the
-- backstop the service cannot bypass. hops caps a corrupt chain so the walk
-- always terminates.
CREATE FUNCTION accounts_hierarchy_guard() RETURNS trigger AS $$
DECLARE
  ancestor uuid;
  parent_currency text;
  hops int := 0;
BEGIN
  IF NEW.parent_id IS NULL THEN
    RETURN NEW;
  END IF;
  IF NEW.parent_id = NEW.id THEN
    RAISE EXCEPTION 'account % cannot be its own parent', NEW.id USING ERRCODE = '23514';
  END IF;
  SELECT currency INTO parent_currency FROM accounts
    WHERE tenant_id = NEW.tenant_id AND id = NEW.parent_id;
  IF NOT FOUND THEN
    -- No such parent in this tenant: let it through so the accounts_parent_fk
    -- foreign key rejects it with a foreign_key_violation (23503), which the
    -- repository maps to ErrParentNotFound rather than ErrInvalidHierarchy.
    -- Without this check, parent_currency stays NULL, and "NULL IS DISTINCT
    -- FROM NEW.currency" is true, so an unknown parent_id would otherwise be
    -- misreported as a currency mismatch (23514) before the FK ever runs.
    RETURN NEW;
  END IF;
  IF parent_currency IS DISTINCT FROM NEW.currency THEN
    RAISE EXCEPTION 'account % currency % does not match parent currency %',
      NEW.id, NEW.currency, parent_currency USING ERRCODE = '23514';
  END IF;
  ancestor := NEW.parent_id;
  WHILE ancestor IS NOT NULL LOOP
    IF ancestor = NEW.id THEN
      RAISE EXCEPTION 'account % parent_id % would create a cycle', NEW.id, NEW.parent_id
        USING ERRCODE = '23514';
    END IF;
    hops := hops + 1;
    IF hops > 10000 THEN
      RAISE EXCEPTION 'account hierarchy too deep or corrupt for %', NEW.id USING ERRCODE = '23514';
    END IF;
    SELECT parent_id INTO ancestor FROM accounts
      WHERE tenant_id = NEW.tenant_id AND id = ancestor;
  END LOOP;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER accounts_hierarchy_guard_trg
  BEFORE INSERT OR UPDATE OF parent_id ON accounts
  FOR EACH ROW EXECUTE FUNCTION accounts_hierarchy_guard();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS accounts_hierarchy_guard_trg ON accounts;
DROP FUNCTION IF EXISTS accounts_hierarchy_guard();
DROP INDEX IF EXISTS accounts_parent_idx;
ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_parent_fk;
ALTER TABLE accounts DROP COLUMN IF EXISTS parent_id;
-- +goose StatementEnd
