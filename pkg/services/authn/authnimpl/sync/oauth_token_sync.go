package sync

import (
	"context"
	"errors"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/infra/tracing"
	"github.com/grafana/grafana/pkg/login/social"
	"github.com/grafana/grafana/pkg/services/auth"
	"github.com/grafana/grafana/pkg/services/authn"
	"github.com/grafana/grafana/pkg/services/oauthtoken"
)

func ProvideOAuthTokenSync(service oauthtoken.OAuthTokenService, sessionService auth.UserTokenService, socialService social.Service, tracer tracing.Tracer) *OAuthTokenSync {
	return &OAuthTokenSync{
		log.New("oauth_token.sync"),
		service,
		sessionService,
		socialService,
		new(singleflight.Group),
		tracer,
	}
}

type OAuthTokenSync struct {
	log               log.Logger
	service           oauthtoken.OAuthTokenService
	sessionService    auth.UserTokenService
	socialService     social.Service
	singleflightGroup *singleflight.Group
	tracer            tracing.Tracer
}

func (s *OAuthTokenSync) SyncOauthTokenHook(ctx context.Context, identity *authn.Identity, _ *authn.Request) error {
	ctx, span := s.tracer.Start(ctx, "oauth.sync.SyncOauthTokenHook")
	defer span.End()

	// only perform oauth token check if identity is a user
	if !identity.ID.IsNamespace(authn.NamespaceUser) {
		return nil
	}

	// Not authenticated through session tokens, so we can skip this hook.
	if identity.SessionToken == nil {
		return nil
	}

	// Not authenticated with a oauth provider, so we can skip this hook.
	if !strings.HasPrefix(identity.GetAuthenticatedBy(), "oauth") {
		return nil
	}

	ctxLogger := s.log.FromContext(ctx).New("userID", identity.ID.ID())

	_, err, _ := s.singleflightGroup.Do(identity.ID.String(), func() (interface{}, error) {
		ctxLogger.Debug("Singleflight request for OAuth token sync")

		updateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()

		_, refreshErr := s.service.TryTokenRefresh(updateCtx, identity)
		if refreshErr != nil {
			if errors.Is(refreshErr, context.Canceled) {
				return nil, nil
			}

			ctxLogger.Error("Failed to refresh OAuth access token", "id", identity.ID, "error", refreshErr)

			if err := s.sessionService.RevokeToken(ctx, identity.SessionToken, false); err != nil {
				ctxLogger.Warn("Failed to revoke session token", "id", identity.ID, "tokenId", identity.SessionToken.Id, "error", err)
			}

			return nil, refreshErr
		}
		return nil, nil
	})

	if err != nil {
		return authn.ErrExpiredAccessToken.Errorf("OAuth access token could not be refreshed: %w", err)
	}

	return nil
}
