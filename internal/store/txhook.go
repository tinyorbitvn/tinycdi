package store

import "context"

// TxHook runs inside every WithTx transaction whose context carries it.
// PreCommit runs after the transaction body succeeded and before COMMIT —
// an error rolls the whole transaction back; Committed runs after COMMIT
// succeeded. The API's audit wrapper uses it to write the audit row in the
// same transaction as the audited mutation (ADR 0010).
type TxHook interface {
	PreCommit(ctx context.Context, tx Tx) error
	Committed()
}

type txHookKey struct{}

// ContextWithTxHook returns ctx carrying h.
func ContextWithTxHook(ctx context.Context, h TxHook) context.Context {
	return context.WithValue(ctx, txHookKey{}, h)
}

// TxHookFromContext returns the hook ctx carries, or nil.
func TxHookFromContext(ctx context.Context) TxHook {
	h, _ := ctx.Value(txHookKey{}).(TxHook)
	return h
}
