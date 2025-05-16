package config

import (
	"testing"

	"github.com/conductorone/baton-sdk/pkg/test"
)

func TestConfigs(t *testing.T) {
	configurationSchema := Config

	testCases := []test.TestCase{
		// Add test cases here.
	}

	test.ExerciseTestCases(t, configurationSchema, ValidateConfig, testCases)
}
