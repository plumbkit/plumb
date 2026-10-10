// Package reports summarises ledger data.
package reports

// Summarise returns the count and sum of charges.
func Summarise(charges []int) (n, sum int) {
	for _, c := range charges {
		sum += c
	}
	return len(charges), sum
}

// StatusLabel maps a numeric status to its label. It is deliberately long:
// its body is larger than any context budget, so a pack must hand it off
// to read_symbol rather than split it.
func StatusLabel(code int) string {
	switch code {
	case 0:
		return "code-000: reserved status label for report row 0; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 1:
		return "code-001: reserved status label for report row 1; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 2:
		return "code-002: reserved status label for report row 2; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 3:
		return "code-003: reserved status label for report row 3; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 4:
		return "code-004: reserved status label for report row 4; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 5:
		return "code-005: reserved status label for report row 5; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 6:
		return "code-006: reserved status label for report row 6; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 7:
		return "code-007: reserved status label for report row 7; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 8:
		return "code-008: reserved status label for report row 8; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 9:
		return "code-009: reserved status label for report row 9; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 10:
		return "code-010: reserved status label for report row 10; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 11:
		return "code-011: reserved status label for report row 11; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 12:
		return "code-012: reserved status label for report row 12; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 13:
		return "code-013: reserved status label for report row 13; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 14:
		return "code-014: reserved status label for report row 14; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 15:
		return "code-015: reserved status label for report row 15; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 16:
		return "code-016: reserved status label for report row 16; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 17:
		return "code-017: reserved status label for report row 17; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 18:
		return "code-018: reserved status label for report row 18; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 19:
		return "code-019: reserved status label for report row 19; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 20:
		return "code-020: reserved status label for report row 20; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 21:
		return "code-021: reserved status label for report row 21; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 22:
		return "code-022: reserved status label for report row 22; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 23:
		return "code-023: reserved status label for report row 23; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 24:
		return "code-024: reserved status label for report row 24; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 25:
		return "code-025: reserved status label for report row 25; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 26:
		return "code-026: reserved status label for report row 26; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 27:
		return "code-027: reserved status label for report row 27; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 28:
		return "code-028: reserved status label for report row 28; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 29:
		return "code-029: reserved status label for report row 29; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 30:
		return "code-030: reserved status label for report row 30; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 31:
		return "code-031: reserved status label for report row 31; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 32:
		return "code-032: reserved status label for report row 32; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 33:
		return "code-033: reserved status label for report row 33; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 34:
		return "code-034: reserved status label for report row 34; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 35:
		return "code-035: reserved status label for report row 35; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 36:
		return "code-036: reserved status label for report row 36; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 37:
		return "code-037: reserved status label for report row 37; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 38:
		return "code-038: reserved status label for report row 38; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 39:
		return "code-039: reserved status label for report row 39; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 40:
		return "code-040: reserved status label for report row 40; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 41:
		return "code-041: reserved status label for report row 41; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 42:
		return "code-042: reserved status label for report row 42; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 43:
		return "code-043: reserved status label for report row 43; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 44:
		return "code-044: reserved status label for report row 44; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 45:
		return "code-045: reserved status label for report row 45; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 46:
		return "code-046: reserved status label for report row 46; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 47:
		return "code-047: reserved status label for report row 47; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 48:
		return "code-048: reserved status label for report row 48; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 49:
		return "code-049: reserved status label for report row 49; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 50:
		return "code-050: reserved status label for report row 50; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 51:
		return "code-051: reserved status label for report row 51; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 52:
		return "code-052: reserved status label for report row 52; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 53:
		return "code-053: reserved status label for report row 53; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 54:
		return "code-054: reserved status label for report row 54; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 55:
		return "code-055: reserved status label for report row 55; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 56:
		return "code-056: reserved status label for report row 56; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 57:
		return "code-057: reserved status label for report row 57; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 58:
		return "code-058: reserved status label for report row 58; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 59:
		return "code-059: reserved status label for report row 59; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 60:
		return "code-060: reserved status label for report row 60; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 61:
		return "code-061: reserved status label for report row 61; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 62:
		return "code-062: reserved status label for report row 62; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 63:
		return "code-063: reserved status label for report row 63; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 64:
		return "code-064: reserved status label for report row 64; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 65:
		return "code-065: reserved status label for report row 65; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 66:
		return "code-066: reserved status label for report row 66; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 67:
		return "code-067: reserved status label for report row 67; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 68:
		return "code-068: reserved status label for report row 68; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 69:
		return "code-069: reserved status label for report row 69; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 70:
		return "code-070: reserved status label for report row 70; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 71:
		return "code-071: reserved status label for report row 71; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 72:
		return "code-072: reserved status label for report row 72; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 73:
		return "code-073: reserved status label for report row 73; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 74:
		return "code-074: reserved status label for report row 74; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 75:
		return "code-075: reserved status label for report row 75; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 76:
		return "code-076: reserved status label for report row 76; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 77:
		return "code-077: reserved status label for report row 77; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 78:
		return "code-078: reserved status label for report row 78; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 79:
		return "code-079: reserved status label for report row 79; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 80:
		return "code-080: reserved status label for report row 80; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 81:
		return "code-081: reserved status label for report row 81; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 82:
		return "code-082: reserved status label for report row 82; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 83:
		return "code-083: reserved status label for report row 83; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 84:
		return "code-084: reserved status label for report row 84; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 85:
		return "code-085: reserved status label for report row 85; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 86:
		return "code-086: reserved status label for report row 86; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 87:
		return "code-087: reserved status label for report row 87; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 88:
		return "code-088: reserved status label for report row 88; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 89:
		return "code-089: reserved status label for report row 89; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 90:
		return "code-090: reserved status label for report row 90; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 91:
		return "code-091: reserved status label for report row 91; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 92:
		return "code-092: reserved status label for report row 92; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 93:
		return "code-093: reserved status label for report row 93; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 94:
		return "code-094: reserved status label for report row 94; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 95:
		return "code-095: reserved status label for report row 95; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 96:
		return "code-096: reserved status label for report row 96; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 97:
		return "code-097: reserved status label for report row 97; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 98:
		return "code-098: reserved status label for report row 98; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 99:
		return "code-099: reserved status label for report row 99; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 100:
		return "code-100: reserved status label for report row 100; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 101:
		return "code-101: reserved status label for report row 101; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 102:
		return "code-102: reserved status label for report row 102; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 103:
		return "code-103: reserved status label for report row 103; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 104:
		return "code-104: reserved status label for report row 104; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 105:
		return "code-105: reserved status label for report row 105; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 106:
		return "code-106: reserved status label for report row 106; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 107:
		return "code-107: reserved status label for report row 107; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 108:
		return "code-108: reserved status label for report row 108; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 109:
		return "code-109: reserved status label for report row 109; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 110:
		return "code-110: reserved status label for report row 110; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 111:
		return "code-111: reserved status label for report row 111; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 112:
		return "code-112: reserved status label for report row 112; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 113:
		return "code-113: reserved status label for report row 113; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 114:
		return "code-114: reserved status label for report row 114; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 115:
		return "code-115: reserved status label for report row 115; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 116:
		return "code-116: reserved status label for report row 116; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 117:
		return "code-117: reserved status label for report row 117; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 118:
		return "code-118: reserved status label for report row 118; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 119:
		return "code-119: reserved status label for report row 119; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 120:
		return "code-120: reserved status label for report row 120; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 121:
		return "code-121: reserved status label for report row 121; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 122:
		return "code-122: reserved status label for report row 122; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 123:
		return "code-123: reserved status label for report row 123; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 124:
		return "code-124: reserved status label for report row 124; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 125:
		return "code-125: reserved status label for report row 125; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 126:
		return "code-126: reserved status label for report row 126; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 127:
		return "code-127: reserved status label for report row 127; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 128:
		return "code-128: reserved status label for report row 128; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	case 129:
		return "code-129: reserved status label for report row 129; padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget padding that keeps this body larger than any context budget"
	default:
		return "unknown"
	}
}
