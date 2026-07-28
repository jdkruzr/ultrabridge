package service

import "strings"

// MaxOperatorLabelLen caps an operator-set device label, in runes. Matches
// syncsvc.MaxDeviceNameLen so the two names a device can carry are bounded the
// same way — but the two normalizers stay separate on purpose: that one cleans
// an untrusted wire envelope, this one cleans operator input from Settings.
const MaxOperatorLabelLen = 128

// normalizeOperatorLabel trims and truncates a label from the UI or REST API.
// Truncation is rune-wise so a multibyte name is never cut mid-character.
// An all-whitespace label normalizes to "", which the stores treat as "clear
// it" — deliberately unlike the wire's device_name, where empty means "leave
// the stored name alone" (an old client that omits the field must not erase a
// known name, but an operator submitting a blank field means to remove theirs).
func normalizeOperatorLabel(label string) string {
	label = strings.TrimSpace(label)
	if r := []rune(label); len(r) > MaxOperatorLabelLen {
		return strings.TrimSpace(string(r[:MaxOperatorLabelLen]))
	}
	return label
}

// displayName picks what to show for a device: the operator's label when set,
// else the device-reported name. Shared by SyncDevice and RemarkableDevice so
// the precedence rule has exactly one definition.
func displayName(label, reported string) string {
	if label != "" {
		return label
	}
	return reported
}
