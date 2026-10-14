package model

import (
	"net/http"
	"strconv"
	"strings"
)

func configuredCacheIdentity(value any) (string, bool, error) {
	provider, ok := value.(CacheIdentityProvider)
	if !ok {
		return "", false, nil
	}
	identity, err := provider.CacheIdentity()
	if err != nil || strings.TrimSpace(identity) == "" {
		return "", false, nil
	}
	return identity, true, nil
}

func httpClientCacheIdentity(client *http.Client) (string, bool, error) {
	if client == nil {
		return "", false, nil
	}
	if identity, ok, err := configuredCacheIdentity(client); ok || err != nil {
		return identity, ok, err
	}
	if client.Timeout != 0 || client.Jar != nil {
		return "", false, nil
	}
	if client.Transport == nil {
		return "", false, nil
	}
	if identity, ok, err := configuredCacheIdentity(client.Transport); ok || err != nil {
		return "custom-http-client-transport:" + identity, ok, err
	}
	return "", false, nil
}

func framedModelIdentity(parts ...string) string {
	var builder strings.Builder
	for _, part := range parts {
		builder.WriteString(strconv.Itoa(len(part)))
		builder.WriteByte(':')
		builder.WriteString(part)
	}
	return builder.String()
}
