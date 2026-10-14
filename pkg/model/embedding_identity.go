package model

import "strconv"

const (
	normalizationL2                      = "l2-normalized"
	normalizationProviderNativeUnchanged = "provider-native-unchanged"
)

func embeddingSpaceIdentity(provider, model string, dimensions int, normalization string) (string, error) {
	if !validConfigValue(provider, maxModelBytes) ||
		!validConfigValue(model, maxModelBytes) ||
		dimensions < 1 || dimensions > MaxVectorDimensions ||
		!validConfigValue(normalization, maxModelBytes) {
		return "", &Error{Kind: ErrInvalidConfig, Operation: "embedding space identity"}
	}
	return framedModelIdentity(
		"embedding-space-v1",
		provider,
		model,
		strconv.Itoa(dimensions),
		normalization,
	), nil
}
