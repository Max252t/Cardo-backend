package boardtx

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/domain"
)

type RankDirection int

const (
	RankAfter RankDirection = iota
	RankBefore
)

type Func func(ctx context.Context, tx Tx) error

type Tx interface {
	Board() domain.Board
	Savepoint(ctx context.Context, fn Func) error
	BoardSettings
	Members
	Events
	ProcessedOps
	Columns
	Cards
}

type BoardSettings interface {
	SetTitle(ctx context.Context, title string) error
	MarkDeleted(ctx context.Context) error
}

type Members interface {
	MemberRole(ctx context.Context, userID uuid.UUID) (domain.Role, error)
	RequireRole(ctx context.Context, userID uuid.UUID, min domain.Role) (domain.Role, error)
}

type Events interface {
	AppendEvent(ctx context.Context, in domain.EventDraft) (domain.Event, error)
	EventBySeq(ctx context.Context, seq int64) (domain.Event, error)
}

type ProcessedOps interface {
	ProcessedOp(ctx context.Context, opID uuid.UUID) (domain.ProcessedOp, bool, error)
	SaveProcessedOp(ctx context.Context, opID, userID uuid.UUID, outcome any) (bool, error)
}

type Columns interface {
	Column(ctx context.Context, id uuid.UUID) (domain.Column, error)
	ColumnIDExists(ctx context.Context, id uuid.UUID) (bool, error)
	ColumnRanks(ctx context.Context) ([]domain.RankedItem, error)
	ColumnRankWindow(ctx context.Context, anchor uuid.UUID, dir RankDirection, exclude uuid.UUID) (anchorRank, neighbour string, found bool, err error)
	ColumnLastRank(ctx context.Context, exclude uuid.UUID) (string, bool, error)
	InsertColumn(ctx context.Context, c domain.Column) (domain.Column, error)
	RenameColumn(ctx context.Context, id uuid.UUID, title string) (domain.Column, error)
	MoveColumn(ctx context.Context, id uuid.UUID, rank string) (domain.Column, error)
	ArchiveColumn(ctx context.Context, id, actorID uuid.UUID) (domain.Column, []uuid.UUID, error)
	RerankColumns(ctx context.Context, items []domain.RankedItem) error
}

type Cards interface {
	Card(ctx context.Context, id uuid.UUID) (domain.Card, error)
	CardIDExists(ctx context.Context, id uuid.UUID) (bool, error)
	CardRanks(ctx context.Context, columnID uuid.UUID) ([]domain.RankedItem, error)
	CardRankWindow(ctx context.Context, columnID, anchor uuid.UUID, dir RankDirection, exclude uuid.UUID) (anchorRank, neighbour string, found bool, err error)
	CardLastRank(ctx context.Context, columnID, exclude uuid.UUID) (string, bool, error)
	InsertCard(ctx context.Context, c domain.Card) (domain.Card, error)
	EditCard(ctx context.Context, id, actorID uuid.UUID, edit domain.CardEdit) (domain.Card, error)
	MoveCard(ctx context.Context, id, actorID, toColumnID uuid.UUID, rank string) (domain.Card, error)
	ArchiveCard(ctx context.Context, id, actorID uuid.UUID) (domain.Card, error)
	RerankCards(ctx context.Context, columnID uuid.UUID, items []domain.RankedItem) error
}

type Roster interface {
	UserByEmail(ctx context.Context, email string) (domain.User, error)
	AddMember(ctx context.Context, userID uuid.UUID, role domain.Role) (domain.Member, error)
	SetMemberRole(ctx context.Context, userID uuid.UUID, role domain.Role) (domain.Member, error)
	RemoveMember(ctx context.Context, userID uuid.UUID) error
	Member(ctx context.Context, userID uuid.UUID) (domain.Member, error)
}

type Invites interface {
	InsertInvite(ctx context.Context, inv domain.Invite) error
	InviteByHash(ctx context.Context, tokenHash string) (domain.Invite, error)
	RevokeInvite(ctx context.Context, id uuid.UUID, at time.Time) error
	ConsumeInvite(ctx context.Context, id uuid.UUID) error
}

type AdminTx interface {
	Tx
	Roster
	Invites
}

type AdminFunc func(ctx context.Context, tx AdminTx) error
