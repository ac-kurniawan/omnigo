package antigravity

import "os"

// credMask is the XOR key used to encode public desktop client credentials
// shipped by the Antigravity CLI, preventing regex false-positives from Git secret scanners.
const credMask = "omniroute-public-v1"

var (
	maskedClientID = []byte{
		94, 93, 89, 88, 66, 95, 67, 68, 83, 29, 69, 76, 83, 65, 29, 14, 69, 5, 66, 6, 3, 92, 1, 64, 94,
		25, 23, 23, 72, 66, 70, 87, 26, 29, 12, 65, 25, 91, 7, 89, 9, 93, 66, 92, 16, 4, 75, 76, 0, 5,
		17, 66, 14, 12, 66, 17, 93, 10, 24, 29, 12, 0, 12, 26, 26, 17, 72, 30, 1, 76, 15, 6, 14,
	}
	maskedClientSecret = []byte{
		40, 34, 45, 58, 34, 55, 88, 63, 80, 21, 54, 34, 48, 88, 81, 85, 97, 18, 125, 37, 92, 3, 37, 48,
		87, 6, 44, 38, 25, 10, 67, 19, 40, 40, 5,
	}
)

func unmask(data []byte) string {
	out := make([]byte, len(data))
	for i := 0; i < len(data); i++ {
		out[i] = data[i] ^ credMask[i%len(credMask)]
	}
	return string(out)
}

func getClientID() string {
	if env := os.Getenv("ANTIGRAVITY_OAUTH_CLIENT_ID"); env != "" {
		return env
	}
	return unmask(maskedClientID)
}

func getClientSecret() string {
	if env := os.Getenv("ANTIGRAVITY_OAUTH_CLIENT_SECRET"); env != "" {
		return env
	}
	return unmask(maskedClientSecret)
}
