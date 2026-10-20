package protocol

import (
	"strings"

	"github.com/google/uuid"
)

const (
	boardChannelPrefix    = "board:"
	presenceChannelPrefix = "presence:"
)

type ChannelKind int

const (
	ChannelUnknown ChannelKind = iota
	ChannelBoard
	ChannelPresence
)

func BoardChannel(boardID uuid.UUID) string {
	return boardChannelPrefix + boardID.String()
}

func PresenceChannel(boardID uuid.UUID) string {
	return presenceChannelPrefix + boardID.String()
}

func ParseChannel(channel string) (ChannelKind, uuid.UUID) {
	if rest, ok := strings.CutPrefix(channel, boardChannelPrefix); ok {
		return parseChannelID(ChannelBoard, rest)
	}
	if rest, ok := strings.CutPrefix(channel, presenceChannelPrefix); ok {
		return parseChannelID(ChannelPresence, rest)
	}
	return ChannelUnknown, uuid.Nil
}

func parseChannelID(kind ChannelKind, raw string) (ChannelKind, uuid.UUID) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return ChannelUnknown, uuid.Nil
	}
	return kind, id
}
