// Copyright (c) TFG Co. All Rights Reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package groups

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/constants"
	"github.com/stretchr/testify/assert"
)

var memoryGroupService *MemoryGroupService

func memoryGroupServiceForTest(t *testing.T) *MemoryGroupService {
	t.Helper()
	memoryGroupsMu.Lock()
	memoryGroups = make(map[string]*MemoryGroup)
	memoryGroupsMu.Unlock()
	return memoryGroupService
}

func TestMain(m *testing.M) {
	mconfig := *&config.NewDefaultPitayaConfig().Groups.Memory
	mconfig.TickDuration = 10 * time.Millisecond
	memoryGroupService = NewMemoryGroupService(mconfig)
	exit := m.Run()
	os.Exit(exit)
}

func TestMemoryCreateDuplicatedGroup(t *testing.T) {
	testCreateDuplicatedGroup(memoryGroupServiceForTest(t), t)
}

func TestMemoryCreateGroup(t *testing.T) {
	testCreateGroup(memoryGroupServiceForTest(t), t)
}

func TestMemoryCreateGroupWithTTL(t *testing.T) {
	testCreateGroupWithTTL(memoryGroupServiceForTest(t), t)
}

func TestMemoryGroupAddMember(t *testing.T) {
	testGroupAddMember(memoryGroupServiceForTest(t), t)
}

func TestMemoryGroupAddDuplicatedMember(t *testing.T) {
	testGroupAddDuplicatedMember(memoryGroupServiceForTest(t), t)
}

func TestMemoryGroupContainsMember(t *testing.T) {
	testGroupContainsMember(memoryGroupServiceForTest(t), t)
}

func TestMemoryRemove(t *testing.T) {
	testRemove(memoryGroupServiceForTest(t), t)
}

func TestMemoryDelete(t *testing.T) {
	testDelete(memoryGroupServiceForTest(t), t)
}

func TestMemoryRemoveAll(t *testing.T) {
	testRemoveAll(memoryGroupServiceForTest(t), t)
}

func TestMemoryCount(t *testing.T) {
	testCount(memoryGroupServiceForTest(t), t)
}

func TestMemoryMembers(t *testing.T) {
	testMembers(memoryGroupServiceForTest(t), t)
}

func TestMemoryMembersPage(t *testing.T) {
	testMembersPage(memoryGroupServiceForTest(t), t)
}

func TestMemoryEmptyUIDRejected(t *testing.T) {
	testEmptyUIDRejected(memoryGroupServiceForTest(t), t)
}

func TestMemoryMembersPageRejectsStoredEmptyUID(t *testing.T) {
	memoryGroupServiceForTest(t)
	groupName := "testMemoryMembersPageRejectsStoredEmptyUID"
	memoryGroupsMu.Lock()
	memoryGroups[groupName] = &MemoryGroup{Uids: []string{"", "b"}}
	memoryGroupsMu.Unlock()
	t.Cleanup(func() {
		memoryGroupsMu.Lock()
		delete(memoryGroups, groupName)
		memoryGroupsMu.Unlock()
	})

	page, err := memoryGroupService.GroupMembersPage(
		context.Background(),
		groupName,
		MemberPageOptions{Limit: 1},
	)
	assert.ErrorIs(t, err, constants.ErrEmptyUID)
	assert.Empty(t, page)
}

func TestMemoryMembersStaySortedAcrossAddAndRemove(t *testing.T) {
	groupService := memoryGroupServiceForTest(t)
	ctx := context.Background()
	const groupName = "testMemoryMembersStaySortedAcrossAddAndRemove"
	assert.NoError(t, groupService.GroupCreate(ctx, groupName))
	for _, uid := range []string{"c", "a", "b"} {
		assert.NoError(t, groupService.GroupAddMember(ctx, groupName, uid))
	}

	members, err := groupService.GroupMembers(ctx, groupName)
	assert.NoError(t, err)
	assert.Equal(t, []string{"a", "b", "c"}, members)

	assert.NoError(t, groupService.GroupRemoveMember(ctx, groupName, "b"))
	members, err = groupService.GroupMembers(ctx, groupName)
	assert.NoError(t, err)
	assert.Equal(t, []string{"a", "c"}, members)
}
