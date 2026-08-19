package groups

import (
	"context"
	"sort"
	"time"
)

type (
	// GroupService has ranking methods
	GroupService interface {
		GroupAddMember(ctx context.Context, groupName, uid string) error
		GroupContainsMember(ctx context.Context, groupName, uid string) (bool, error)
		GroupCountMembers(ctx context.Context, groupName string) (int, error)
		GroupCreate(ctx context.Context, groupName string) error
		GroupCreateWithTTL(ctx context.Context, groupName string, ttlTime time.Duration) error
		GroupDelete(ctx context.Context, groupName string) error
		GroupMembers(ctx context.Context, groupName string) ([]string, error)
		GroupRemoveAll(ctx context.Context, groupName string) error
		GroupRemoveMember(ctx context.Context, groupName, uid string) error
		GroupRenewTTL(ctx context.Context, groupName string) error
	}
)

func elementIndex(slice []string, element string) (int, bool) {
	index := sort.SearchStrings(slice, element)
	return index, index < len(slice) && slice[index] == element
}
