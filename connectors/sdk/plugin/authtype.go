package plugin

import (
	"github.com/asker/asker/connectors/sdk"
	pluginv1 "github.com/asker/asker/platform/proto/gen/go/asker/plugin/v1"
)

// authTypeToProto maps an sdk.AuthType to its wire enum. Unknown values map to
// AUTH_TYPE_UNSPECIFIED so a future sdk auth type never silently masquerades as
// a known one.
func authTypeToProto(a sdk.AuthType) pluginv1.AuthType {
	switch a {
	case sdk.AuthNone:
		return pluginv1.AuthType_AUTH_NONE
	case sdk.AuthOAuth2:
		return pluginv1.AuthType_AUTH_OAUTH2
	case sdk.AuthToken:
		return pluginv1.AuthType_AUTH_TOKEN
	default:
		return pluginv1.AuthType_AUTH_TYPE_UNSPECIFIED
	}
}

// authTypeFromProto is the inverse of authTypeToProto. The unspecified and any
// unknown enum value default to sdk.AuthNone (the safest default: no credential
// flow is run for an auth type the client cannot interpret).
func authTypeFromProto(a pluginv1.AuthType) sdk.AuthType {
	switch a {
	case pluginv1.AuthType_AUTH_OAUTH2:
		return sdk.AuthOAuth2
	case pluginv1.AuthType_AUTH_TOKEN:
		return sdk.AuthToken
	case pluginv1.AuthType_AUTH_NONE, pluginv1.AuthType_AUTH_TYPE_UNSPECIFIED:
		return sdk.AuthNone
	default:
		return sdk.AuthNone
	}
}
