package connector

import (
	"testing"
)

// TestGenerateRandomPassword verifies that the password generation function works correctly.
func TestGenerateRandomPassword(t *testing.T) {
	// Generate passwords of different lengths
	for _, length := range []int{8, 16, 24} {
		password, err := generateRandomPassword(length)
		if err != nil {
			t.Fatalf("Failed to generate random password of length %d: %v", length, err)
		}

		// Check the length
		if len(password) != length {
			t.Errorf("Expected password length %d, got %d", length, len(password))
		}

		// Verify password is not empty
		if password == "" {
			t.Error("Generated password is empty")
		}
	}
}
