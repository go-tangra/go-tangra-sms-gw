package render

import "unicode/utf16"

// Encodings.
const (
	GSM  = "gsm-03-38"
	UTF8 = "utf-8"
)

// limits are the cumulative character capacities of 1..10 parts (carrier
// specification, appendix 2); utf-8 travels as UCS-2.
var limits = map[string][]int{
	GSM:  {160, 306, 459, 612, 765, 918, 1071, 1224, 1377, 1530},
	UTF8: {70, 134, 201, 268, 335, 402, 469, 536, 603, 670},
}

// Characters counts UTF-16 code units, as the source counter does.
func Characters(text string) int { return len(utf16.Encode([]rune(text))) }

func table(encoding string) []int {
	if l, ok := limits[encoding]; ok {
		return l
	}
	return limits[UTF8]
}

// Parts is the number of SMS parts text needs (0 for empty text, at most 10).
func Parts(text, encoding string) int {
	n := Characters(text)
	if n == 0 {
		return 0
	}
	l := table(encoding)
	for i, max := range l {
		if n <= max {
			return i + 1
		}
	}
	return len(l)
}

// Limit is the character ceiling of the part count text currently fills.
func Limit(text, encoding string) int {
	l := table(encoding)
	return l[min(max(Parts(text, encoding), 1), len(l))-1]
}
