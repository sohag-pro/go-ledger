# go-ledger gRPC client example

Run the server (it serves gRPC on `GRPC_ADDR`, default `:9091`), then:

    go run ./examples/grpc-client

Expected output (ids will differ):

    posted transaction <uuid> (replayed=false); cash balance = 10000 USD

## grpcurl smoke

Server reflection is enabled, so grpcurl needs no proto files:

    grpcurl -plaintext localhost:9091 list
    grpcurl -plaintext localhost:9091 ledger.v1.LedgerService/ListAccounts
    grpcurl -plaintext -d '{"name":"Cash","type":"asset","currency":"USD"}' \
      localhost:9091 ledger.v1.LedgerService/CreateAccount

To send an idempotency key, add metadata:

    grpcurl -plaintext -H 'idempotency-key: demo-1' -d '{ ... }' \
      localhost:9091 ledger.v1.LedgerService/PostTransaction
