package namespace

import (
	"errors"
	"flywheel/common"
	"flywheel/domain"
	"flywheel/persistence"
	"flywheel/security"
	"fmt"
	"github.com/fundwit/go-commons/types"
	"github.com/jinzhu/gorm"
	"github.com/sony/sonyflake"
	"time"
)

var (
	idWorker = sonyflake.NewSonyflake(sonyflake.Settings{})
)

func CreateGroup(c *domain.GroupCreating, sec *security.Context) (*domain.Group, error) {
	now := time.Now()
	g := domain.Group{ID: common.NextId(idWorker), Name: c.Name, Identifier: c.Identifier, NextWorkId: 1, CreateTime: now, Creator: sec.Identity.ID}
	r := domain.GroupMember{GroupID: g.ID, MemberId: sec.Identity.ID, Role: domain.RoleOwner, CreateTime: now}
	err := persistence.ActiveDataSourceManager.GormDB().Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(g).Error; err != nil {
			return err
		}
		if err := tx.Create(r).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &g, nil
}

func QueryGroupRole(groupId types.ID, sec *security.Context) (string, error) {
	gm := domain.GroupMember{GroupID: groupId, MemberId: sec.Identity.ID}
	db := persistence.ActiveDataSourceManager.GormDB()
	var founds []domain.GroupMember
	if err := db.Model(domain.GroupMember{}).Where(&gm).Find(&founds).Error; err != nil || founds == nil || len(founds) == 0 {
		return "", err
	}
	return founds[0].Role, nil
}

func NextWorkIdentifier(groupId types.ID, tx *gorm.DB) (string, error) {
	group := domain.Group{}
	if err := tx.Where(&domain.Group{ID: groupId}).First(&group).Error; err != nil {
		return "", err
	}

	// consume current value
	nextWorkID := fmt.Sprintf("%s-%d", group.Identifier, group.NextWorkId)
	// generate next value
	db := tx.Model(&domain.Group{}).Where(&domain.Group{ID: groupId, NextWorkId: group.NextWorkId}).
		Update("next_work_id", group.NextWorkId+1)
	if db.Error != nil {
		return "", db.Error
	}
	if db.RowsAffected != 1 {
		return "", errors.New("concurrent modification")
	}
	return nextWorkID, nil
}
