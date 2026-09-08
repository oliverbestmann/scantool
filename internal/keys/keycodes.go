package keys

// Linux input event types and values from <linux/input-event-codes.h>.
const (
	evKey = 0x01 // EV_KEY

	valueUp     = 0
	valueDown   = 1
	valueRepeat = 2
)

// keyNames maps the Linux keycode to the KEY_* name used by libinput, and to
// the rune the daemon works with. Only the keys that can plausibly be bound to
// an action are listed; everything else is ignored.
var keyNames = []struct {
	code uint16
	name string
	key  rune
}{
	{2, "KEY_1", '1'}, {3, "KEY_2", '2'}, {4, "KEY_3", '3'}, {5, "KEY_4", '4'},
	{6, "KEY_5", '5'}, {7, "KEY_6", '6'}, {8, "KEY_7", '7'}, {9, "KEY_8", '8'},
	{10, "KEY_9", '9'}, {11, "KEY_0", '0'},

	{16, "KEY_Q", 'q'}, {17, "KEY_W", 'w'}, {18, "KEY_E", 'e'}, {19, "KEY_R", 'r'},
	{20, "KEY_T", 't'}, {21, "KEY_Y", 'y'}, {22, "KEY_U", 'u'}, {23, "KEY_I", 'i'},
	{24, "KEY_O", 'o'}, {25, "KEY_P", 'p'},

	{30, "KEY_A", 'a'}, {31, "KEY_S", 's'}, {32, "KEY_D", 'd'}, {33, "KEY_F", 'f'},
	{34, "KEY_G", 'g'}, {35, "KEY_H", 'h'}, {36, "KEY_J", 'j'}, {37, "KEY_K", 'k'},
	{38, "KEY_L", 'l'},

	{44, "KEY_Z", 'z'}, {45, "KEY_X", 'x'}, {46, "KEY_C", 'c'}, {47, "KEY_V", 'v'},
	{48, "KEY_B", 'b'}, {49, "KEY_N", 'n'}, {50, "KEY_M", 'm'},

	{28, "KEY_ENTER", '\n'}, {57, "KEY_SPACE", ' '}, {1, "KEY_ESC", 0x1b},

	// A numeric keypad is a practical way to drive the scanner, so the
	// keypad digits map to the same runes as the number row.
	{79, "KEY_KP1", '1'}, {80, "KEY_KP2", '2'}, {81, "KEY_KP3", '3'},
	{75, "KEY_KP4", '4'}, {76, "KEY_KP5", '5'}, {77, "KEY_KP6", '6'},
	{71, "KEY_KP7", '7'}, {72, "KEY_KP8", '8'}, {73, "KEY_KP9", '9'},
	{82, "KEY_KP0", '0'}, {96, "KEY_KPENTER", '\n'},
}

var (
	runeByCode  = map[uint16]rune{}
	runeByName  = map[string]rune{}
	codesByRune = map[rune][]uint16{}
)

func init() {
	for _, k := range keyNames {
		runeByCode[k.code] = k.key
		runeByName[k.name] = k.key
		codesByRune[k.key] = append(codesByRune[k.key], k.code)
	}
}

// CodesForRune returns the keycodes that produce a rune. A rune can have more
// than one, for example the number row and the keypad digits.
func CodesForRune(key rune) []uint16 { return codesByRune[key] }

// RuneForCode maps a Linux keycode to a rune. Unmapped keys return false.
func RuneForCode(code uint16) (rune, bool) {
	r, ok := runeByCode[code]
	return r, ok
}

// RuneForName maps a KEY_* name to a rune. Unknown names return false.
func RuneForName(name string) (rune, bool) {
	r, ok := runeByName[name]
	return r, ok
}
