package storage

import "testing"

func TestNormalizeISBN(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"bare ISBN-13", "9780306406157", "9780306406157"},
		{"bare ISBN-10", "0306406152", "0306406152"},
		{"hyphenated ISBN-13", "978-0-306-40615-7", "9780306406157"},
		{"hyphenated ISBN-10", "0-306-40615-2", "0306406152"},
		{"spaced groups", "0 306 40615 2", "0306406152"},
		{"urn:isbn: prefix", "urn:isbn:9780306406157", "9780306406157"},
		{"urn:isbn: prefix, ISBN-10", "urn:isbn:0306406152", "0306406152"},
		{"ISBN marker", "ISBN 9780306406157", "9780306406157"},
		{"ISBN marker, colon", "isbn:0306406152", "0306406152"},
		{"lower-case check digit", "0 306 40615 x", "030640615X"},
		{"surrounded by the publisher's prose", "ISBN 978-0-00-000000-0 (ebook)", "9780000000000"},
		{"marked ISBN-10 with prose beside it", "ISBN 0306406152 (paperback)", "0306406152"},
		{"grouped ISBN-10 with prose beside it", "0-306-40615-2 (pbk.)", "0306406152"},
		// The plainest thing a publisher writes in an ISBN slot, and the
		// one a corroborating-shape rule would refuse
		{"bare ISBN-10 with prose beside it", "0306406152 (pbk.)", "0306406152"},
		{"labelled ISBN-13", "ISBN-13: 978-0-306-40615-7", "9780306406157"},
		{"labelled ISBN-10", "ISBN-10: 0306406152", "0306406152"},

		{"empty", "", ""},
		{"no ISBN at all", "Not available", ""},
		{"a UUID", "urn:uuid:0e8e3f8a-4b1c-4d2e-9f3a-5b6c7d8e9f01", ""},
		{"an all-digit UUID", "12345678-1234-5678-1234-567812345678", ""},
		{"fourteen digits", "12345678901234", ""},
		{"nine digits", "123456789", ""},
		{"X anywhere but the tenth position", "03064061X2", ""},
		{"X after thirteen digits", "978030640615X", ""},
		{"X on its own", "X", ""},
		{"doubled separator", "0--306406152", ""},
		// A space is a run byte, so a following digit joins the run rather
		// than starting a new one and the whole thing fails to validate.
		// Pinned as the known edge of the maximal-run design it is: it
		// loses an ISBN, and never yields a wrong one
		{"a digit-led word after the ISBN", "978-0-306-40615-7 2nd ed.", ""},
		{"two ISBNs separated by a space", "9780306406157 0306406152", ""},
		// Separated by anything else, the first run stands on its own
		{"two ISBNs separated by a comma", "9780306406157, 0306406152", "9780306406157"},
		// The digits are the only shape read; en dashes are not hyphens
		{"unicode dashes", "978–0–306–40615–7", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeISBN(tt.in); got != tt.want {
				t.Errorf("NormalizeISBN(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
