package ops_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

func (f *fixture) storedOutcomes(t *testing.T, opID uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, f.pool.QueryRow(context.Background(), `SELECT count(*) FROM processed_ops WHERE op_id = $1`, opID).Scan(&n))
	return n
}

func TestForbiddenNackPersistenceDependsOnMembership(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	viewer := f.member(t, domain.RoleViewer)
	outsider := testutil.CreateUser(t, f.store, testutil.UniqueEmail("outsider"))
	payload := protocol.CardCreate{CardID: uuid.New(), ColumnID: col, Title: "x"}

	outsiderOp := uuid.New()
	out := f.applyAs(t, outsider.ID, outsiderOp, protocol.OpCardCreate, payload)
	require.NotNil(t, out.Nack)
	assert.Equal(t, protocol.ReasonForbidden, out.Nack.Reason)
	assert.Zero(t, f.storedOutcomes(t, outsiderOp), "non-members must not be able to write processed_ops")

	viewerOp := uuid.New()
	out = f.applyAs(t, viewer.ID, viewerOp, protocol.OpCardCreate, payload)
	require.NotNil(t, out.Nack)
	assert.Equal(t, protocol.ReasonForbidden, out.Nack.Reason)
	assert.Equal(t, 1, f.storedOutcomes(t, viewerOp), "members with an insufficient role keep a replayable nack")
}

func TestStoredOutcomeIsVersioned(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	opID := uuid.New()
	out := f.applyAs(t, f.owner.ID, opID, protocol.OpCardCreate, protocol.CardCreate{CardID: uuid.New(), ColumnID: col, Title: "x"})
	require.NotNil(t, out.Event)

	var version int
	require.NoError(t, f.pool.QueryRow(context.Background(), `SELECT (outcome->>'v')::int FROM processed_ops WHERE op_id = $1`, opID).Scan(&version))
	assert.Equal(t, 1, version)

	legacy := uuid.New()
	_, err := f.pool.Exec(context.Background(),
		`INSERT INTO processed_ops (op_id, board_id, user_id, outcome) VALUES ($1, $2, $3, '{"kind":"event","seq":1}')`,
		legacy, f.board.ID, f.owner.ID)
	require.NoError(t, err)
	op := mustOp(t, protocol.OpCardCreate, legacy, protocol.CardCreate{CardID: uuid.New(), ColumnID: col, Title: "x"})
	_, err = f.proc.Apply(context.Background(), f.owner.ID, f.board.ID, op)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "version")

	future := uuid.New()
	_, err = f.pool.Exec(context.Background(),
		`INSERT INTO processed_ops (op_id, board_id, user_id, outcome) VALUES ($1, $2, $3, '{"v":2,"kind":"event","seq":1}')`,
		future, f.board.ID, f.owner.ID)
	require.NoError(t, err)
	op = mustOp(t, protocol.OpCardCreate, future, protocol.CardCreate{CardID: uuid.New(), ColumnID: col, Title: "x"})
	_, err = f.proc.Apply(context.Background(), f.owner.ID, f.board.ID, op)
	require.Error(t, err)
}
