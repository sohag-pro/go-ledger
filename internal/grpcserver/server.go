package grpcserver

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"

	"github.com/sohag-pro/go-ledger/internal/auth"
	"github.com/sohag-pro/go-ledger/internal/domain"
	ledgerv1 "github.com/sohag-pro/go-ledger/internal/genproto/ledger/v1"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/paging"
)

// Page limit defaults and maxima for the gRPC handlers. These mirror the REST
// huma tags (`minimum`/`maximum` struct tags in internal/api/accounts.go and
// internal/api/audit.go), which are string literals and cannot be shared as Go
// constants, so keep the two in sync by hand.
const (
	defaultAccountsLimit = 100
	maxAccountsLimit     = 500
	defaultPageLimit     = 50
	maxPageLimit         = 200
)

// maxPostingsPerTransaction bounds how many postings a single
// PostTransaction RPC may submit (Task 5.1, audit A2.2). REST has no
// explicit count check of its own: it is implicitly bounded by
// api.MaxRequestBodyBytes (64 KB), and JSON's per-field overhead means a
// 64 KB body cannot smuggle many more than about 100 postings anyway. gRPC's
// protobuf encoding is dense enough that the same 64 KB (or even
// maxGRPCRecvMsgBytes below) could carry many more, so gRPC needs its own
// explicit count check rather than relying on the message-size cap alone.
// 100 matches the audit's stated cap; there is no existing domain-level
// maximum to align with instead (domain.Transaction.Validate only enforces a
// MINIMUM of two postings and that they sum to zero per currency).
const maxPostingsPerTransaction = 100

// maxGRPCRecvMsgBytes replaces gRPC's 4 MiB default incoming-message limit
// with a deliberate one (Task 5.1, audit A2.2), parallel to REST's 64 KB
// MaxRequestBodyBytes cap. 1 MiB is comfortably above the largest legitimate
// request this service ever receives (a PostTransaction at
// maxPostingsPerTransaction postings, each with an account id, an amount,
// and a description, is on the order of tens of KB) while still being a
// small, deliberate fraction of the library default, so an oversized or
// malicious payload is rejected by the transport before it is ever
// unmarshaled or reaches a handler.
const maxGRPCRecvMsgBytes = 1 << 20 // 1 MiB

// clampLimit returns def when requested is <= 0, maxVal when requested
// exceeds maxVal, and requested otherwise. It guards the gRPC handlers
// against a caller requesting an unbounded scan, mirroring the REST layer's
// huma maximum tags.
func clampLimit(requested, def, maxVal int) int {
	if requested <= 0 {
		return def
	}
	if requested > maxVal {
		return maxVal
	}
	return requested
}

// Deps are the shared services the gRPC handlers call, the same ones the REST
// layer uses, plus the resolver the auth interceptor uses to authenticate
// every call and derive its tenant (see ADR-012), and the rate limiter the
// rate-limit interceptor enforces (Task 5.1, audit A2.2). RateLimiter must be
// the SAME *auth.Limiter instance passed to api.Deps.RateLimiter in
// cmd/server/main.go, not a second one: see rateLimitUnaryInterceptor's doc
// comment (internal/grpcserver/interceptors.go) for why a shared instance is
// the deliberate choice.
type Deps struct {
	Accounts     *ledger.AccountService
	Transactions *ledger.TransactionService
	Audit        *ledger.AuditService
	Auth         *auth.Resolver
	RateLimiter  *auth.Limiter
}

// Server implements the generated LedgerServiceServer as a thin adapter: it
// translates protobuf to domain types, calls the shared services, and translates
// back. It holds no business rules.
type Server struct {
	ledgerv1.UnimplementedLedgerServiceServer
	accounts *ledger.AccountService
	txns     *ledger.TransactionService
	audit    *ledger.AuditService
}

// NewServer builds the LedgerService implementation from the shared services.
func NewServer(d Deps) *Server {
	return &Server{accounts: d.Accounts, txns: d.Transactions, audit: d.Audit}
}

// NewGRPCServer builds a *grpc.Server with the interceptor chain, the
// LedgerService, server reflection (so grpcurl works), and the health service.
// Every LedgerService call must authenticate through d.Auth (ADR-012); the
// gRPC health check is exempt so liveness probes work without an API key.
// Reflection is likewise left open: it only describes the service, it does
// not call it, so there is nothing to authenticate.
//
// The rate-limit interceptor is chained AFTER authUnaryInterceptor
// deliberately (Task 5.1, audit A2.2): it reads the key authUnaryInterceptor
// resolved into the context, so it must run downstream of it, the same
// dependency HumaMiddleware has on the REST auth middleware.
//
// grpc.MaxRecvMsgSize replaces the library's 4 MiB default with
// maxGRPCRecvMsgBytes, a deliberate bound parallel to REST's
// api.MaxRequestBodyBytes (see maxGRPCRecvMsgBytes's own doc comment).
//
// TLS/loopback decision (Task 5.1, audit A2.2, ADR-015 Phase 5): this server
// is NOT given transport credentials here. It is deployed loopback-only (see
// cfg.grpcAddr's default in cmd/server/main.go, "127.0.0.1:9091"), the same
// posture as Postgres ("listen_addresses = 'localhost'") and the metrics
// endpoint (ADR-004): every caller reaching it is already on the box, so
// there is no network hop for TLS to protect. Exposing this service to a
// caller off-box requires terminating TLS in front of it first, either an
// nginx grpc_pass proxy (matching how REST already terminates TLS at nginx)
// or grpc.Creds(credentials.NewTLS(...)) added here; shipping it in the
// clear on a public interface, which was the audit finding, is exactly what
// the loopback default now prevents by construction rather than by
// operator discipline.
func NewGRPCServer(d Deps, log *slog.Logger) *grpc.Server {
	if log == nil {
		log = slog.Default()
	}
	s := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.MaxRecvMsgSize(maxGRPCRecvMsgBytes),
		grpc.ChainUnaryInterceptor(
			recoveryUnaryInterceptor(log),
			loggingUnaryInterceptor(log),
			authUnaryInterceptor(d.Auth, log),
			rateLimitUnaryInterceptor(d.RateLimiter, log),
		),
	)
	ledgerv1.RegisterLedgerServiceServer(s, NewServer(d))

	hs := health.NewServer()
	hs.SetServingStatus("", healthv1.HealthCheckResponse_SERVING)
	healthv1.RegisterHealthServer(s, hs)

	reflection.Register(s)
	return s
}

// --- translation helpers ---

// toProtoAccount translates a domain.Account to its protobuf shape. MinBalance
// (Task 5.5, audit A1.5) is left at its zero value (0) when unset: proto3 has
// no distinct "absent" for a scalar int64, so a gRPC caller cannot tell "no
// floor" apart from "floor of exactly zero" the way the REST API's nullable
// min_balance can (see the proto's own doc comment on Account.min_balance).
func toProtoAccount(a domain.Account) *ledgerv1.Account {
	pa := &ledgerv1.Account{Id: a.ID, Name: a.Name, Type: a.Type.String(), Currency: string(a.Currency), Status: string(a.Status)}
	if a.MinBalance != nil {
		pa.MinBalance = *a.MinBalance
	}
	return pa
}

// toProtoTransaction translates a domain.Transaction to its protobuf shape.
// There is deliberately no transaction-level currency: a convert transaction
// spans two currencies, so a single top-level currency could never label
// every posting correctly (the same reasoning as the REST TransactionBody).
// Each Posting carries its own currency instead.
func toProtoTransaction(t domain.Transaction) *ledgerv1.Transaction {
	postings := make([]*ledgerv1.Posting, 0, len(t.Postings))
	for _, p := range t.Postings {
		postings = append(postings, &ledgerv1.Posting{
			AccountId:   p.AccountID,
			Amount:      p.Amount.Amount(),
			Description: p.Description,
			Currency:    string(p.Amount.Currency()),
		})
	}
	pt := &ledgerv1.Transaction{Id: t.ID, Postings: postings}
	// reverses_transaction_id (Task 4.2, audit A1.2) is set only when t is
	// itself a reversal; left at its zero value ("") otherwise.
	if t.ReversesTransactionID != nil {
		pt.ReversesTransactionId = *t.ReversesTransactionID
	}
	// reference (Task 4.3, audit A1.3) is left at its zero value ("") when t
	// was posted without one.
	if t.Reference != nil {
		pt.Reference = *t.Reference
	}
	// effective_at (Task 4.3, audit A1.3) is always set by the time t reaches
	// here: CreateTransaction resolves the read-time fallback to created_at
	// itself (see postgres.txRepo.CreateTransaction and
	// Repository.transactionFromRow). Defensive nil check anyway, the same
	// as toTransactionBody's REST counterpart.
	if t.EffectiveAt != nil {
		pt.EffectiveAt = t.EffectiveAt.UTC().Format(time.RFC3339Nano)
	}
	return pt
}

// toProtoFXDetail translates a domain.FXDetail to its protobuf shape. Nil
// stays nil: an ordinary (non-converting) transaction has no FX detail.
func toProtoFXDetail(fx *domain.FXDetail) *ledgerv1.FXDetail {
	if fx == nil {
		return nil
	}
	return &ledgerv1.FXDetail{
		SourceAmount:    fx.SourceAmount,
		ConvertedAmount: fx.ConvertedAmount,
		MidRateE8:       fx.MidRateE8,
		AppliedE8:       fx.AppliedE8,
		SpreadBps:       fx.SpreadBps,
		RateSource:      fx.RateSource,
		EffectiveAt:     fx.EffectiveAt.UTC().Format(time.RFC3339Nano),
		RateId:          fx.RateID,
	}
}

func toProtoAuditEntry(e domain.AuditEntry) *ledgerv1.AuditEntry {
	return &ledgerv1.AuditEntry{
		Id:            e.ID,
		Action:        e.Action,
		TransactionId: e.TransactionID,
		Actor:         e.Actor,
		Before:        string(e.Before),
		After:         string(e.After),
		CreatedAt:     e.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// --- handlers ---

// CreateAccount creates an account for the calling tenant. min_balance (Task
// 5.5, audit A1.5) is optional: 0 means no floor was requested, mirroring
// toProtoAccount's own doc comment on the same ambiguity.
func (s *Server) CreateAccount(ctx context.Context, req *ledgerv1.CreateAccountRequest) (*ledgerv1.CreateAccountResponse, error) {
	at, err := domain.ParseAccountType(req.Type)
	if err != nil {
		return nil, toStatus(err)
	}
	acct := &domain.Account{Name: req.Name, Type: at, Currency: domain.Currency(req.Currency)}
	if req.MinBalance != 0 {
		acct.MinBalance = &req.MinBalance
	}
	if err := s.accounts.Create(ctx, tenantFrom(ctx), acct); err != nil {
		return nil, toStatus(err)
	}
	return &ledgerv1.CreateAccountResponse{Account: toProtoAccount(*acct)}, nil
}

// GetAccount fetches a single account by id for the calling tenant.
func (s *Server) GetAccount(ctx context.Context, req *ledgerv1.GetAccountRequest) (*ledgerv1.GetAccountResponse, error) {
	acct, err := s.accounts.Get(ctx, tenantFrom(ctx), req.Id)
	if err != nil {
		return nil, toStatus(err)
	}
	return &ledgerv1.GetAccountResponse{Account: toProtoAccount(acct)}, nil
}

// SetAccountStatus freezes, closes, or reactivates an account for the
// calling tenant (Task 5.5, audit A1.5), mirroring POST
// /v1/accounts/{id}/status, and returns the updated account.
func (s *Server) SetAccountStatus(ctx context.Context, req *ledgerv1.SetAccountStatusRequest) (*ledgerv1.SetAccountStatusResponse, error) {
	acct, err := s.accounts.SetStatus(ctx, tenantFrom(ctx), req.Id, domain.AccountStatus(req.Status))
	if err != nil {
		return nil, toStatus(err)
	}
	return &ledgerv1.SetAccountStatusResponse{Account: toProtoAccount(acct)}, nil
}

// ListAccounts lists accounts for the calling tenant, most recent first.
func (s *Server) ListAccounts(ctx context.Context, req *ledgerv1.ListAccountsRequest) (*ledgerv1.ListAccountsResponse, error) {
	limit := clampLimit(int(req.Limit), defaultAccountsLimit, maxAccountsLimit)
	accts, err := s.accounts.List(ctx, tenantFrom(ctx), limit)
	if err != nil {
		return nil, toStatus(err)
	}
	out := make([]*ledgerv1.Account, 0, len(accts))
	for _, a := range accts {
		out = append(out, toProtoAccount(a))
	}
	return &ledgerv1.ListAccountsResponse{Accounts: out}, nil
}

// GetBalance returns an account's current balance, derived from its postings.
func (s *Server) GetBalance(ctx context.Context, req *ledgerv1.GetBalanceRequest) (*ledgerv1.GetBalanceResponse, error) {
	bal, err := s.accounts.Balance(ctx, tenantFrom(ctx), req.AccountId)
	if err != nil {
		return nil, toStatus(err)
	}
	return &ledgerv1.GetBalanceResponse{AccountId: req.AccountId, Amount: bal.Amount(), Currency: string(bal.Currency())}, nil
}

// GetStatement returns a page of an account's posting history with a running
// balance, keyset paged by cursor.
func (s *Server) GetStatement(ctx context.Context, req *ledgerv1.GetStatementRequest) (*ledgerv1.GetStatementResponse, error) {
	after, err := paging.DecodeCursor(req.Cursor)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	limit := clampLimit(int(req.Limit), defaultPageLimit, maxPageLimit)
	acct, entries, err := s.accounts.Statement(ctx, tenantFrom(ctx), req.AccountId, after, limit)
	if err != nil {
		return nil, toStatus(err)
	}
	out := make([]*ledgerv1.StatementEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, &ledgerv1.StatementEntry{
			Id:             e.ID,
			TransactionId:  e.TransactionID,
			Amount:         e.Amount.Amount(),
			RunningBalance: e.RunningBalance.Amount(),
			Description:    e.Description,
			CreatedAt:      e.CreatedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	resp := &ledgerv1.GetStatementResponse{AccountId: acct.ID, Currency: string(acct.Currency), Entries: out}
	if limit > 0 && len(entries) == limit {
		last := entries[len(entries)-1]
		resp.NextCursor = paging.EncodeCursor(last.CreatedAt, last.ID)
	}
	return resp, nil
}

// PostTransaction posts a balanced set of postings as a new transaction. The
// idempotency-key metadata is required (ADR-012): when it was already used
// with the same request body, the original result is replayed instead of
// posting again.
func (s *Server) PostTransaction(ctx context.Context, req *ledgerv1.PostTransactionRequest) (*ledgerv1.PostTransactionResponse, error) {
	key := idempotencyKeyFrom(ctx)
	if key == "" {
		return nil, status.Error(codes.InvalidArgument, "idempotency-key metadata is required")
	}
	if len(req.Postings) > maxPostingsPerTransaction {
		return nil, status.Errorf(codes.InvalidArgument,
			"too many postings: got %d, max %d", len(req.Postings), maxPostingsPerTransaction)
	}

	currency := domain.Currency(req.Currency)
	postings := make([]domain.Posting, 0, len(req.Postings))
	for _, p := range req.Postings {
		amount, err := domain.NewMoney(p.Amount, currency)
		if err != nil {
			return nil, toStatus(err)
		}
		postings = append(postings, domain.Posting{AccountID: p.AccountId, Amount: amount, Description: p.Description})
	}
	txn := &domain.Transaction{Postings: postings}
	// reference and effective_at (Task 4.3, audit A1.3) are both optional:
	// an empty proto string means "not supplied", the same convention
	// idempotency-key metadata already uses, so an unset reference stays nil
	// rather than becoming a pointer to "" (which Transaction.Validate would
	// reject as ErrInvalidReference).
	if req.Reference != "" {
		txn.Reference = &req.Reference
	}
	if req.EffectiveAt != "" {
		effectiveAt, err := time.Parse(time.RFC3339Nano, req.EffectiveAt)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "effective_at must be an RFC3339 timestamp")
		}
		txn.EffectiveAt = &effectiveAt
	}
	idem := &domain.Idempotency{Key: key}
	replayed, err := s.txns.Post(ctx, tenantFrom(ctx), txn, idem)
	if err != nil {
		return nil, toStatus(err)
	}
	return &ledgerv1.PostTransactionResponse{Transaction: toProtoTransaction(*txn), Replayed: replayed}, nil
}

// Convert exchanges source_amount from the from account's currency into the
// to account's currency at the tenant's current FX rate, and posts the four
// resulting legs, mirroring POST /v1/transactions/convert (ADR-014). Like
// PostTransaction, the idempotency-key metadata is required (ADR-012): a
// repeat with the same key replays the original conversion instead of
// converting again.
func (s *Server) Convert(ctx context.Context, req *ledgerv1.ConvertRequest) (*ledgerv1.ConvertResponse, error) {
	key := idempotencyKeyFrom(ctx)
	if key == "" {
		return nil, status.Error(codes.InvalidArgument, "idempotency-key metadata is required")
	}

	creq := ledger.ConvertRequest{
		FromAccountID: req.FromAccount,
		ToAccountID:   req.ToAccount,
		SourceAmount:  req.SourceAmount,
	}
	idem := &domain.Idempotency{Key: key}
	txn, replayed, err := s.txns.Convert(ctx, tenantFrom(ctx), creq, idem)
	if err != nil {
		return nil, toStatus(err)
	}
	return &ledgerv1.ConvertResponse{
		Transaction: toProtoTransaction(*txn),
		Fx:          toProtoFXDetail(txn.FX),
		Replayed:    replayed,
	}, nil
}

// GetTransaction fetches a single transaction by id for the calling tenant.
func (s *Server) GetTransaction(ctx context.Context, req *ledgerv1.GetTransactionRequest) (*ledgerv1.GetTransactionResponse, error) {
	txn, err := s.txns.Get(ctx, tenantFrom(ctx), req.Id)
	if err != nil {
		return nil, toStatus(err)
	}
	return &ledgerv1.GetTransactionResponse{Transaction: toProtoTransaction(txn)}, nil
}

// parseTransactionFilter builds a domain.TransactionFilter from
// ListTransactionsRequest's from/to/reference fields (Task 4.4, audit A7.2).
// from and to are optional RFC3339 timestamps; an empty string leaves that
// side of the filter unset, mirroring the REST layer's identical helper
// (internal/api/transactions.go's parseTransactionFilter): kept as its own
// small copy here rather than shared, the same as this package's page-limit
// constants above, since a proto string field and a huma query tag are not
// worth threading one shared helper through.
func parseTransactionFilter(from, to, reference string) (domain.TransactionFilter, error) {
	var filter domain.TransactionFilter
	if from != "" {
		t, err := time.Parse(time.RFC3339Nano, from)
		if err != nil {
			return filter, errors.New("from must be an RFC3339 timestamp")
		}
		filter.From = &t
	}
	if to != "" {
		t, err := time.Parse(time.RFC3339Nano, to)
		if err != nil {
			return filter, errors.New("to must be an RFC3339 timestamp")
		}
		filter.To = &t
	}
	if reference != "" {
		filter.Reference = &reference
	}
	return filter, nil
}

// ListTransactions returns a page of the tenant's transactions, newest
// first, keyset paged by cursor, optionally filtered by a from/to
// created_at range and/or an exact reference match (Task 4.4, audit A7.2),
// mirroring GET /v1/transactions. Export has no gRPC equivalent: a
// streaming CSV export does not fit gRPC's single-response model, so it
// stays REST-only (see internal/api/transactions.go's export handler).
func (s *Server) ListTransactions(ctx context.Context, req *ledgerv1.ListTransactionsRequest) (*ledgerv1.ListTransactionsResponse, error) {
	filter, err := parseTransactionFilter(req.From, req.To, req.Reference)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	after, err := paging.DecodeCursor(req.Cursor)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	limit := clampLimit(int(req.Limit), defaultPageLimit, maxPageLimit)
	// Requested as limit+1, not limit: see paging.Page's doc comment for why
	// that is what lets hasMore below detect a next page without a second
	// round trip.
	rows, err := s.txns.ListTransactions(ctx, tenantFrom(ctx), filter, after, limit+1)
	if err != nil {
		return nil, toStatus(err)
	}
	page, hasMore := paging.Page(rows, limit)
	out := make([]*ledgerv1.Transaction, 0, len(page))
	for _, item := range page {
		out = append(out, toProtoTransaction(item.Transaction))
	}
	resp := &ledgerv1.ListTransactionsResponse{Transactions: out}
	if hasMore {
		last := page[len(page)-1]
		resp.NextCursor = paging.EncodeCursor(last.CreatedAt, last.Transaction.ID)
	}
	return resp, nil
}

// ReverseTransaction posts the negated legs of the transaction named by
// req.OriginalTransactionId as a new, linked transaction, mirroring POST
// /v1/transactions/{id}/reverse (Task 4.2, audit A1.2). Idempotent: a
// repeat call for the same original returns the SAME reversal, with
// already_reversed = true, instead of posting a second one.
func (s *Server) ReverseTransaction(ctx context.Context, req *ledgerv1.ReverseTransactionRequest) (*ledgerv1.ReverseTransactionResponse, error) {
	reversal, alreadyReversed, err := s.txns.ReverseTransaction(ctx, tenantFrom(ctx), req.OriginalTransactionId)
	if err != nil {
		return nil, toStatus(err)
	}
	return &ledgerv1.ReverseTransactionResponse{
		Transaction:     toProtoTransaction(*reversal),
		AlreadyReversed: alreadyReversed,
	}, nil
}

// GetTransactionAudit returns the audit trail entries recorded for a transaction.
func (s *Server) GetTransactionAudit(ctx context.Context, req *ledgerv1.GetTransactionAuditRequest) (*ledgerv1.GetTransactionAuditResponse, error) {
	entries, err := s.audit.ByTransaction(ctx, tenantFrom(ctx), req.TransactionId)
	if err != nil {
		return nil, toStatus(err)
	}
	out := make([]*ledgerv1.AuditEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, toProtoAuditEntry(e))
	}
	return &ledgerv1.GetTransactionAuditResponse{Entries: out}, nil
}

// GetAccountAudit returns a page of audit trail entries for an account, keyset
// paged by cursor.
func (s *Server) GetAccountAudit(ctx context.Context, req *ledgerv1.GetAccountAuditRequest) (*ledgerv1.GetAccountAuditResponse, error) {
	after, err := paging.DecodeCursor(req.Cursor)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	limit := clampLimit(int(req.Limit), defaultPageLimit, maxPageLimit)
	entries, err := s.audit.ByAccount(ctx, tenantFrom(ctx), req.AccountId, after, limit)
	if err != nil {
		return nil, toStatus(err)
	}
	out := make([]*ledgerv1.AuditEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, toProtoAuditEntry(e))
	}
	resp := &ledgerv1.GetAccountAuditResponse{Entries: out}
	if limit > 0 && len(entries) == limit {
		last := entries[len(entries)-1]
		resp.NextCursor = paging.EncodeCursor(last.CreatedAt, last.ID)
	}
	return resp, nil
}

// idempotencyKeyFrom reads the idempotency-key from incoming gRPC metadata,
// mirroring the REST Idempotency-Key header. Empty when absent.
func idempotencyKeyFrom(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	if vals := md.Get("idempotency-key"); len(vals) > 0 {
		return vals[0]
	}
	return ""
}
