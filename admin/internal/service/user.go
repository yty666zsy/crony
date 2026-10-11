package service

import (
	"errors"
	"fmt"

	"github.com/tmnhs/crony/admin/internal/model/request"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/crypt"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"github.com/tmnhs/crony/common/pkg/logger"
	"gorm.io/gorm"
)

// dummyHash 用于"用户不存在"时也执行一次 bcrypt 校验，
// 使登录失败路径的耗时与"用户存在但口令错误"接近，降低用户名枚举风险。
const dummyHash = "$2a$12$C6UzMDM.H6dfI/f/IKcEe.oJ8yq0dM0dJ0m9Yl8Z9Tz1s6pQ4v3Wq"

type UserService struct {
}

var DefaultUserService = new(UserService)

// Login 校验用户名/口令。
//
// 与旧实现的区别：
//   - 旧实现把密码拼进 SQL `where username = ? and password = md5(?)`，
//     依赖无盐 MD5 比对；现在改为"按用户名查出哈希 → bcrypt/兼容 MD5 校验"；
//   - 存量账号的 MD5 哈希在校验通过后会被透明升级为 bcrypt（needRehash）；
//   - 无论成功失败都不会把口令哈希返回给上层。
func (us *UserService) Login(username, password string) (u *models.User, err error) {
	u = new(models.User)
	err = dbclient.GetMysqlDB().Table(models.CronyUserTableName).
		Where("username = ?", username).First(u).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 用户不存在时也走一次 bcrypt 比对，避免时序差异暴露账号是否存在。
			crypt.VerifyPassword(dummyHash, password)
			return nil, nil
		}
		return nil, err
	}

	ok, needRehash := crypt.VerifyPassword(u.Password, password)
	if !ok {
		return nil, nil
	}
	if needRehash {
		us.upgradeLegacyHash(u.ID, password)
	}
	u.Password = ""
	return u, nil
}

// upgradeLegacyHash 把历史 MD5 口令无损升级为 bcrypt。
// 升级失败不影响本次登录（仅记录日志），下次登录会再次尝试。
func (us *UserService) upgradeLegacyHash(userID int, plainPassword string) {
	hashed, err := crypt.HashPassword(plainPassword)
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[user_login] 口令哈希升级失败 user_id=%d error=%s", userID, err.Error()))
		return
	}
	err = dbclient.GetMysqlDB().Table(models.CronyUserTableName).
		Where("id = ?", userID).Update("password", hashed).Error
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[user_login] 口令哈希升级写库失败 user_id=%d error=%s", userID, err.Error()))
		return
	}
	logger.GetLogger().Info(fmt.Sprintf("[user_login] 已将 user_id=%d 的遗留 MD5 口令升级为 bcrypt", userID))
}

func (us *UserService) FindByUserName(username string) (u *models.User, err error) {
	u = new(models.User)
	err = dbclient.GetMysqlDB().Table(models.CronyUserTableName).Where("username = ? ", username).First(u).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	return
}

// Count 返回用户总数。用于判断是否处于"首个管理员引导"阶段。
func (us *UserService) Count() (int64, error) {
	var total int64
	err := dbclient.GetMysqlDB().Table(models.CronyUserTableName).Count(&total).Error
	return total, err
}

// ChangePassword 校验原口令后写入新口令（bcrypt）。
func (us *UserService) ChangePassword(userId int, oldPassword, newPassword string) error {
	current := new(models.User)
	err := dbclient.GetMysqlDB().Table(models.CronyUserTableName).
		Select("id", "password").Where("id = ?", userId).First(current).Error
	if err != nil {
		return err
	}
	if ok, _ := crypt.VerifyPassword(current.Password, oldPassword); !ok {
		return errors.New("原密码不正确")
	}
	hashed, err := crypt.HashPassword(newPassword)
	if err != nil {
		return err
	}
	return dbclient.GetMysqlDB().Table(models.CronyUserTableName).
		Where("id = ?", userId).Update("password", hashed).Error
}

func (us *UserService) Search(s *request.ReqUserSearch) ([]models.User, int64, error) {
	db := dbclient.GetMysqlDB().Table(models.CronyUserTableName)
	if len(s.UserName) > 0 {
		db = db.Where("username like ?", s.UserName+"%")
	}
	// 注意：GORM 的条件必须回链（db = db.Where(...)），
	// 旧代码写成 db.Where(...) 而不接返回值，导致筛选条件被静默丢弃。
	if len(s.Email) > 0 {
		db = db.Where("email = ?", s.Email)
	}
	if s.Role > 0 {
		db = db.Where("role = ?", s.Role)
	}
	if s.ID > 0 {
		db = db.Where("id = ?", s.ID)
	}
	users := make([]models.User, 0)
	var total int64
	err := db.Count(&total).Error
	if err != nil {
		return nil, 0, err
	}
	err = db.Select("id", "username", "email", "role", "created", "updated").Limit(s.PageSize).Offset((s.Page - 1) * s.PageSize).Find(&users).Error
	if err != nil {
		return nil, 0, err
	}

	return users, total, nil
}
