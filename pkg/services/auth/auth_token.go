package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"strings"
	"time"

	"github.com/grafana/grafana/pkg/infra/localcache"
	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/infra/serverlock"
	"github.com/grafana/grafana/pkg/models"
	"github.com/grafana/grafana/pkg/services/sqlstore"
	"github.com/grafana/grafana/pkg/services/user"
	"github.com/grafana/grafana/pkg/setting"
	"github.com/grafana/grafana/pkg/util"
)

const ServiceName = "UserAuthTokenService"

var getTime = time.Now

const (
	cacheTTL         = 30 * time.Second
	urgentRotateTime = 1 * time.Minute
)

func ProvideUserAuthTokenService(
	sqlStore *sqlstore.SQLStore, serverLockService *serverlock.ServerLockService,
	cfg *setting.Cfg, cache *localcache.CacheService,
) *UserAuthTokenService {
	s := &UserAuthTokenService{
		SQLStore:          sqlStore,
		ServerLockService: serverLockService,
		Cfg:               cfg,
		log:               log.New("auth"),
		cache:             cache,
	}
	return s
}

type UserAuthTokenService struct {
	SQLStore          *sqlstore.SQLStore
	ServerLockService *serverlock.ServerLockService
	Cfg               *setting.Cfg
	log               log.Logger
	cache             *localcache.CacheService
}

type ActiveAuthTokenService struct {
	cfg      *setting.Cfg
	sqlStore sqlstore.Store
}

func ProvideActiveAuthTokenService(cfg *setting.Cfg, sqlStore sqlstore.Store) *ActiveAuthTokenService {
	return &ActiveAuthTokenService{
		cfg:      cfg,
		sqlStore: sqlStore,
	}
}

func (a *ActiveAuthTokenService) ActiveTokenCount(ctx context.Context) (int64, error) {
	var count int64
	var err error
	err = a.sqlStore.WithDbSession(ctx, func(dbSession *sqlstore.DBSession) error {
		var model userAuthToken
		count, err = dbSession.Where(`created_at > ? AND rotated_at > ? AND revoked_at = 0`,
			getTime().Add(-a.cfg.LoginMaxLifetime).Unix(),
			getTime().Add(-a.cfg.LoginMaxInactiveLifetime).Unix()).
			Count(&model)

		return err
	})

	return count, err
}

func (s *UserAuthTokenService) CreateToken(ctx context.Context, user *user.User, clientIP net.IP, userAgent string) (*models.UserToken, error) {
	token, err := util.RandomHex(16)
	if err != nil {
		return nil, err
	}

	hashedToken := hashToken(token)

	now := getTime().Unix()
	clientIPStr := clientIP.String()
	if len(clientIP) == 0 {
		clientIPStr = ""
	}

	userAuthToken := userAuthToken{
		UserId:        user.ID,
		AuthToken:     hashedToken,
		PrevAuthToken: hashedToken,
		ClientIp:      clientIPStr,
		UserAgent:     userAgent,
		RotatedAt:     now,
		CreatedAt:     now,
		UpdatedAt:     now,
		SeenAt:        0,
		RevokedAt:     0,
		AuthTokenSeen: false,
	}

	err = s.SQLStore.WithDbSession(ctx, func(dbSession *sqlstore.DBSession) error {
		_, err = dbSession.Insert(&userAuthToken)
		return err
	})

	if err != nil {
		return nil, err
	}

	userAuthToken.UnhashedToken = token

	s.log.Debug("user auth token created", "tokenId", userAuthToken.Id, "userId", userAuthToken.UserId, "clientIP", userAuthToken.ClientIp, "userAgent", userAuthToken.UserAgent, "authToken", userAuthToken.AuthToken)

	var userToken models.UserToken
	err = userAuthToken.toUserToken(&userToken)

	return &userToken, err
}

func (s *UserAuthTokenService) LookupToken(ctx context.Context, unhashedToken string) (*models.UserToken, error) {
	model, hashedToken, err := s.lookupCachedToken(ctx, unhashedToken)
	if err != nil {
		return nil, err
	}

	if model.RevokedAt > 0 {
		s.log.Debug("user token has been revoked", "user ID", model.UserId, "token ID", model.Id)
		return nil, &models.TokenRevokedError{
			UserID:  model.UserId,
			TokenID: model.Id,
		}
	}

	if model.CreatedAt <= s.createdAfterParam() || model.RotatedAt <= s.rotatedAfterParam() {
		s.log.Debug("user token has expired", "user ID", model.UserId, "token ID", model.Id)
		return nil, &models.TokenExpiredError{
			UserID:  model.UserId,
			TokenID: model.Id,
		}
	}

	if model.AuthToken != hashedToken && model.PrevAuthToken == hashedToken && model.AuthTokenSeen {
		modelCopy := *model
		modelCopy.AuthTokenSeen = false
		expireBefore := getTime().Add(-urgentRotateTime).Unix()

		var affectedRows int64
		err = s.SQLStore.WithTransactionalDbSession(ctx, func(dbSession *sqlstore.DBSession) error {
			affectedRows, err = dbSession.Where("id = ? AND prev_auth_token = ? AND rotated_at < ?",
				modelCopy.Id,
				modelCopy.PrevAuthToken,
				expireBefore).
				AllCols().Update(&modelCopy)

			return err
		})

		if err != nil {
			return nil, err
		}

		if affectedRows == 0 {
			s.log.Debug("prev seen token unchanged", "tokenId", model.Id, "userId", model.UserId, "clientIP", model.ClientIp, "userAgent", model.UserAgent, "authToken", model.AuthToken)
		} else {
			s.log.Debug("prev seen token", "tokenId", model.Id, "userId", model.UserId, "clientIP", model.ClientIp, "userAgent", model.UserAgent, "authToken", model.AuthToken)
		}
	}

	if !model.AuthTokenSeen && model.AuthToken == hashedToken {
		modelCopy := *model
		modelCopy.AuthTokenSeen = true
		modelCopy.SeenAt = getTime().Unix()

		var affectedRows int64
		err = s.SQLStore.WithTransactionalDbSession(ctx, func(dbSession *sqlstore.DBSession) error {
			affectedRows, err = dbSession.Where("id = ? AND auth_token = ?",
				modelCopy.Id,
				modelCopy.AuthToken).
				AllCols().Update(&modelCopy)

			return err
		})

		if err != nil {
			return nil, err
		}

		if affectedRows == 1 {
			model = &modelCopy
		}

		if affectedRows == 0 {
			s.log.Debug("seen wrong token", "tokenId", model.Id, "userId", model.UserId, "clientIP", model.ClientIp, "userAgent", model.UserAgent, "authToken", model.AuthToken)
		} else {
			s.log.Debug("seen token", "tokenId", model.Id, "userId", model.UserId, "clientIP", model.ClientIp, "userAgent", model.UserAgent, "authToken", model.AuthToken)
		}
	}

	model.UnhashedToken = unhashedToken

	var userToken models.UserToken
	err = model.toUserToken(&userToken)

	return &userToken, err
}

type cachedToken struct {
	hashedToken   string
	userAuthtoken userAuthToken
}

// lookupCachedToken will check if the raw token is cached. With this we don't have to hash the raw token and do a db lookup
// on every call. If no cached token is found we call lookupToken
func (s *UserAuthTokenService) lookupCachedToken(ctx context.Context, rawToken string) (*userAuthToken, string, error) {
	key := cacheKey(rawToken)
	cached, ok := s.cache.Get(key)
	if !ok {
		s.log.Debug("no auth token found in cache")
		token, hashedToken, err := s.lookupToken(ctx, rawToken)
		if err != nil {
			return nil, "", err
		}
		s.log.Debug("cache auth token")
		s.cache.Set(key, cachedToken{hashedToken, *token}, cacheTTL)
		return token, hashedToken, nil
	}

	s.log.Debug("auth token found in cache")
	token := cached.(cachedToken)
	return &token.userAuthtoken, token.hashedToken, nil
}

// lookupToken hashes the raw token and do a db lookup for user auth token
func (s *UserAuthTokenService) lookupToken(ctx context.Context, rawToken string) (*userAuthToken, string, error) {
	hashedToken := hashToken(rawToken)

	var err error
	var exists bool
	var model userAuthToken
	err = s.SQLStore.WithDbSession(ctx, func(dbSession *sqlstore.DBSession) error {
		exists, err = dbSession.Where("(auth_token = ? OR prev_auth_token = ?)",
			hashedToken,
			hashedToken).
			Get(&model)
		return err
	})
	if err != nil {
		return nil, "", err
	}

	if !exists {
		return nil, "", models.ErrUserTokenNotFound
	}

	return &model, hashedToken, err
}

func (s *UserAuthTokenService) TryRotateToken(ctx context.Context, token *models.UserToken,
	clientIP net.IP, userAgent string) (bool, error) {
	if token == nil {
		return false, nil
	}

	model, err := userAuthTokenFromUserToken(token)
	if err != nil {
		return false, err
	}

	now := getTime()

	var needsRotation bool
	rotatedAt := time.Unix(model.RotatedAt, 0)
	if model.AuthTokenSeen {
		needsRotation = rotatedAt.Before(now.Add(-time.Duration(s.Cfg.TokenRotationIntervalMinutes) * time.Minute))
	} else {
		needsRotation = rotatedAt.Before(now.Add(-urgentRotateTime))
	}

	if !needsRotation {
		return false, nil
	}

	s.log.Debug("token needs rotation", "tokenId", model.Id, "authTokenSeen", model.AuthTokenSeen, "rotatedAt", rotatedAt)

	clientIPStr := clientIP.String()
	if len(clientIP) == 0 {
		clientIPStr = ""
	}
	newToken, err := util.RandomHex(16)
	if err != nil {
		return false, err
	}
	hashedToken := hashToken(newToken)

	// very important that auth_token_seen is set after the prev_auth_token = case when ... for mysql to function correctly
	sql := `
		UPDATE user_auth_token
		SET
			seen_at = 0,
			user_agent = ?,
			client_ip = ?,
			prev_auth_token = case when auth_token_seen = ? then auth_token else prev_auth_token end,
			auth_token = ?,
			auth_token_seen = ?,
			rotated_at = ?
		WHERE id = ? AND (auth_token_seen = ? OR rotated_at < ?)`

	var affected int64
	err = s.SQLStore.WithTransactionalDbSession(ctx, func(dbSession *sqlstore.DBSession) error {
		res, err := dbSession.Exec(sql, userAgent, clientIPStr, s.SQLStore.Dialect.BooleanStr(true), hashedToken,
			s.SQLStore.Dialect.BooleanStr(false), now.Unix(), model.Id, s.SQLStore.Dialect.BooleanStr(true),
			now.Add(-30*time.Second).Unix())
		if err != nil {
			return err
		}

		affected, err = res.RowsAffected()
		return err
	})

	if err != nil {
		return false, err
	}

	s.log.Debug("auth token rotated", "affected", affected, "auth_token_id", model.Id, "userId", model.UserId)
	if affected > 0 {
		model.UnhashedToken = newToken
		if err := model.toUserToken(token); err != nil {
			return false, err
		}
		return true, nil
	}

	return false, nil
}

func (s *UserAuthTokenService) RevokeToken(ctx context.Context, token *models.UserToken, soft bool) error {
	if token == nil {
		return models.ErrUserTokenNotFound
	}

	model, err := userAuthTokenFromUserToken(token)
	if err != nil {
		return err
	}

	var rowsAffected int64

	if soft {
		model.RevokedAt = getTime().Unix()
		err = s.SQLStore.WithDbSession(ctx, func(dbSession *sqlstore.DBSession) error {
			rowsAffected, err = dbSession.ID(model.Id).Update(model)
			return err
		})
	} else {
		err = s.SQLStore.WithDbSession(ctx, func(dbSession *sqlstore.DBSession) error {
			rowsAffected, err = dbSession.Delete(model)
			return err
		})
	}

	if err != nil {
		return err
	}

	if rowsAffected == 0 {
		s.log.Debug("user auth token not found/revoked", "tokenId", model.Id, "userId", model.UserId, "clientIP", model.ClientIp, "userAgent", model.UserAgent)
		return models.ErrUserTokenNotFound
	}

	s.log.Debug("user auth token revoked", "tokenId", model.Id, "userId", model.UserId, "clientIP", model.ClientIp, "userAgent", model.UserAgent, "soft", soft)

	return nil
}

func (s *UserAuthTokenService) RevokeAllUserTokens(ctx context.Context, userId int64) error {
	return s.SQLStore.WithDbSession(ctx, func(dbSession *sqlstore.DBSession) error {
		sql := `DELETE from user_auth_token WHERE user_id = ?`
		res, err := dbSession.Exec(sql, userId)
		if err != nil {
			return err
		}

		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}

		s.log.Debug("all user tokens for user revoked", "userId", userId, "count", affected)

		return err
	})
}

func (s *UserAuthTokenService) BatchRevokeAllUserTokens(ctx context.Context, userIds []int64) error {
	return s.SQLStore.WithTransactionalDbSession(ctx, func(dbSession *sqlstore.DBSession) error {
		if len(userIds) == 0 {
			return nil
		}

		user_id_params := strings.Repeat(",?", len(userIds)-1)
		sql := "DELETE from user_auth_token WHERE user_id IN (?" + user_id_params + ")"

		params := []interface{}{sql}
		for _, v := range userIds {
			params = append(params, v)
		}

		res, err := dbSession.Exec(params...)
		if err != nil {
			return err
		}

		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}

		s.log.Debug("all user tokens for given users revoked", "usersCount", len(userIds), "count", affected)

		return err
	})
}

func (s *UserAuthTokenService) GetUserToken(ctx context.Context, userId, userTokenId int64) (*models.UserToken, error) {
	var result models.UserToken
	err := s.SQLStore.WithDbSession(ctx, func(dbSession *sqlstore.DBSession) error {
		var token userAuthToken
		exists, err := dbSession.Where("id = ? AND user_id = ?", userTokenId, userId).Get(&token)
		if err != nil {
			return err
		}

		if !exists {
			return models.ErrUserTokenNotFound
		}

		return token.toUserToken(&result)
	})

	return &result, err
}

func (s *UserAuthTokenService) GetUserTokens(ctx context.Context, userId int64) ([]*models.UserToken, error) {
	result := []*models.UserToken{}
	err := s.SQLStore.WithDbSession(ctx, func(dbSession *sqlstore.DBSession) error {
		var tokens []*userAuthToken
		err := dbSession.Where("user_id = ? AND created_at > ? AND rotated_at > ? AND revoked_at = 0",
			userId,
			s.createdAfterParam(),
			s.rotatedAfterParam()).
			Find(&tokens)
		if err != nil {
			return err
		}

		for _, token := range tokens {
			var userToken models.UserToken
			if err := token.toUserToken(&userToken); err != nil {
				return err
			}
			result = append(result, &userToken)
		}

		return nil
	})

	return result, err
}

func (s *UserAuthTokenService) GetUserRevokedTokens(ctx context.Context, userId int64) ([]*models.UserToken, error) {
	result := []*models.UserToken{}
	err := s.SQLStore.WithDbSession(ctx, func(dbSession *sqlstore.DBSession) error {
		var tokens []*userAuthToken
		err := dbSession.Where("user_id = ? AND revoked_at > 0", userId).Find(&tokens)
		if err != nil {
			return err
		}

		for _, token := range tokens {
			var userToken models.UserToken
			if err := token.toUserToken(&userToken); err != nil {
				return err
			}
			result = append(result, &userToken)
		}

		return nil
	})

	return result, err
}

func (s *UserAuthTokenService) createdAfterParam() int64 {
	return getTime().Add(-s.Cfg.LoginMaxLifetime).Unix()
}

func (s *UserAuthTokenService) rotatedAfterParam() int64 {
	return getTime().Add(-s.Cfg.LoginMaxInactiveLifetime).Unix()
}

// cacheKey is the token with a prefix
func cacheKey(token string) string {
	return "auth-token-" + token
}

func hashToken(token string) string {
	hashBytes := sha256.Sum256([]byte(token + setting.SecretKey))
	return hex.EncodeToString(hashBytes[:])
}
