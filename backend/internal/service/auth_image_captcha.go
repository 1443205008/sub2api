package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html"
	"math/big"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	registrationImageCaptchaPurpose = "registration_image_captcha"
	registrationImageCaptchaTTL     = 30 * time.Minute
	registrationImageCaptchaWidth   = 132
	registrationImageCaptchaHeight  = 44
	registrationImageCaptchaLength  = 4
	registrationImageCaptchaChars   = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
)

type RegistrationImageCaptcha struct {
	CaptchaID string
	ImageData string
	ExpiresIn int
}

type registrationImageCaptchaClaims struct {
	Nonce      string `json:"nonce"`
	AnswerHash string `json:"answer_hash"`
	Purpose    string `json:"purpose"`
	jwt.RegisteredClaims
}

func (s *AuthService) GenerateRegistrationImageCaptcha(_ context.Context) (*RegistrationImageCaptcha, error) {
	if s.cfg == nil || strings.TrimSpace(s.cfg.JWT.Secret) == "" {
		return nil, ErrServiceUnavailable
	}

	answer, err := randomCaptchaString(registrationImageCaptchaLength)
	if err != nil {
		return nil, fmt.Errorf("generate captcha answer: %w", err)
	}
	nonce, err := randomHexString(12)
	if err != nil {
		return nil, fmt.Errorf("generate captcha nonce: %w", err)
	}

	now := time.Now()
	expiresAt := now.Add(registrationImageCaptchaTTL)
	answerHash := registrationImageCaptchaHMAC(s.cfg.JWT.Secret, nonce, answer, expiresAt.Unix())
	claims := &registrationImageCaptchaClaims{
		Nonce:      nonce,
		AnswerHash: answerHash,
		Purpose:    registrationImageCaptchaPurpose,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	captchaID, err := token.SignedString([]byte(s.cfg.JWT.Secret))
	if err != nil {
		return nil, fmt.Errorf("sign captcha token: %w", err)
	}

	svg, err := renderRegistrationImageCaptchaSVG(answer)
	if err != nil {
		return nil, fmt.Errorf("render captcha image: %w", err)
	}

	return &RegistrationImageCaptcha{
		CaptchaID: captchaID,
		ImageData: "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(svg)),
		ExpiresIn: int(registrationImageCaptchaTTL.Seconds()),
	}, nil
}

func (s *AuthService) VerifyRegistrationImageCaptcha(ctx context.Context, captchaID, captchaCode string) error {
	if !s.IsRegistrationImageCaptchaEnabled(ctx) {
		return nil
	}
	if strings.TrimSpace(captchaID) == "" || strings.TrimSpace(captchaCode) == "" {
		return ErrRegistrationImageCaptchaRequired
	}
	if s.cfg == nil || strings.TrimSpace(s.cfg.JWT.Secret) == "" {
		return ErrServiceUnavailable
	}

	parser := jwt.NewParser(jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Name}))
	token, err := parser.ParseWithClaims(strings.TrimSpace(captchaID), &registrationImageCaptchaClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(s.cfg.JWT.Secret), nil
	})
	if err != nil {
		return ErrRegistrationImageCaptchaInvalid
	}

	claims, ok := token.Claims.(*registrationImageCaptchaClaims)
	if !ok || !token.Valid || claims.Purpose != registrationImageCaptchaPurpose || claims.ExpiresAt == nil {
		return ErrRegistrationImageCaptchaInvalid
	}

	expected := registrationImageCaptchaHMAC(
		s.cfg.JWT.Secret,
		claims.Nonce,
		normalizeRegistrationImageCaptchaCode(captchaCode),
		claims.ExpiresAt.Time.Unix(),
	)
	if !hmac.Equal([]byte(expected), []byte(claims.AnswerHash)) {
		return ErrRegistrationImageCaptchaInvalid
	}
	return nil
}

func registrationImageCaptchaHMAC(secret, nonce, answer string, expiresAt int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(nonce))
	mac.Write([]byte{0})
	mac.Write([]byte(normalizeRegistrationImageCaptchaCode(answer)))
	mac.Write([]byte{0})
	mac.Write([]byte(fmt.Sprintf("%d", expiresAt)))
	return hex.EncodeToString(mac.Sum(nil))
}

func normalizeRegistrationImageCaptchaCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

func randomCaptchaString(length int) (string, error) {
	if length <= 0 {
		length = registrationImageCaptchaLength
	}
	var builder strings.Builder
	for i := 0; i < length; i++ {
		index, err := secureRandomInt(len(registrationImageCaptchaChars))
		if err != nil {
			return "", err
		}
		builder.WriteByte(registrationImageCaptchaChars[index])
	}
	return builder.String(), nil
}

func secureRandomInt(max int) (int, error) {
	if max <= 0 {
		return 0, fmt.Errorf("invalid max: %d", max)
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(max)))
	if err != nil {
		return 0, err
	}
	return int(n.Int64()), nil
}

func renderRegistrationImageCaptchaSVG(answer string) (string, error) {
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-label="captcha">`, registrationImageCaptchaWidth, registrationImageCaptchaHeight, registrationImageCaptchaWidth, registrationImageCaptchaHeight))
	builder.WriteString(`<rect width="100%" height="100%" rx="8" fill="#f8fafc"/>`)

	for i := 0; i < 5; i++ {
		x1, err := secureRandomInt(registrationImageCaptchaWidth)
		if err != nil {
			return "", err
		}
		y1, err := secureRandomInt(registrationImageCaptchaHeight)
		if err != nil {
			return "", err
		}
		x2, err := secureRandomInt(registrationImageCaptchaWidth)
		if err != nil {
			return "", err
		}
		y2, err := secureRandomInt(registrationImageCaptchaHeight)
		if err != nil {
			return "", err
		}
		builder.WriteString(fmt.Sprintf(`<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="#cbd5e1" stroke-width="1.4"/>`, x1, y1, x2, y2))
	}

	for i, char := range answer {
		rotation, err := secureRandomInt(31)
		if err != nil {
			return "", err
		}
		rotation -= 15
		yOffset, err := secureRandomInt(7)
		if err != nil {
			return "", err
		}
		x := 16 + i*26
		y := 31 + yOffset - 3
		builder.WriteString(fmt.Sprintf(
			`<text x="%d" y="%d" transform="rotate(%d %d %d)" font-family="Arial, Helvetica, sans-serif" font-size="24" font-weight="700" fill="#1f2937">%s</text>`,
			x, y, rotation, x, y, html.EscapeString(string(char)),
		))
	}

	for i := 0; i < 14; i++ {
		cx, err := secureRandomInt(registrationImageCaptchaWidth)
		if err != nil {
			return "", err
		}
		cy, err := secureRandomInt(registrationImageCaptchaHeight)
		if err != nil {
			return "", err
		}
		radius, err := secureRandomInt(3)
		if err != nil {
			return "", err
		}
		builder.WriteString(fmt.Sprintf(`<circle cx="%d" cy="%d" r="%d" fill="#94a3b8" opacity="0.45"/>`, cx, cy, radius+1))
	}

	builder.WriteString(`</svg>`)
	return builder.String(), nil
}
