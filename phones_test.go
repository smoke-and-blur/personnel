package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormPhone(t *testing.T) {
	cases := map[string]string{
		"+380671234567":       "+380671234567",
		"0671234567":          "+380671234567",
		"671234567":           "+380671234567",
		"+38 (067) 123-45-67": "+380671234567",
		"дурня":               "",
		"":                    "",
		"12345":               "",
	}
	for in, want := range cases {
		if got := normPhone(in); got != want {
			t.Errorf("normPhone(%q) = %q, треба %q", in, got, want)
		}
	}
}

func TestPibKey(t *testing.T) {
	a := pibKey("ГРАНЧАК Олег Мико'лайович")
	b := pibKey("  гранчак   олег   миколайович ")
	if a != b {
		t.Errorf("ключі мають збігатися: %q vs %q", a, b)
	}
	if a != "гранчак олег миколайович" {
		t.Errorf("несподіваний ключ: %q", a)
	}
}

func TestPickColumns(t *testing.T) {
	head := []string{"№", "Статус", "Логін", "ПІБ", "Посада",
		"Номер основного телефону", "Номер додаткового телефону"}
	pib, phone := pickColumns(head)
	if pib != 3 || phone != 5 {
		t.Errorf("колонки = %d/%d, треба 3/5", pib, phone)
	}
}

// Реального списку в репозиторії немає (він у .gitignore) — якщо лежить поруч,
// перевіряємо на ньому читання xlsx, інакше пропускаємо.
func TestReadRealListIfPresent(t *testing.T) {
	m, _ := filepath.Glob("Учасники*.xlsx")
	if len(m) == 0 {
		t.Skip("списку немає")
	}
	if _, err := os.Stat(m[0]); err != nil {
		t.Skip("списку немає")
	}
	rows, err := readXLSX(m[0])
	if err != nil {
		t.Fatalf("читання: %v", err)
	}
	if len(rows) < 2 {
		t.Fatalf("порожній аркуш")
	}
	ip, it := pickColumns(rows[0])
	if ip < 0 || it < 0 {
		t.Fatalf("колонок ПІБ/телефон не знайдено в %v", rows[0])
	}
	var ok int
	for _, r := range rows[1:] {
		if cell(r, ip) != "" && normPhone(cell(r, it)) != "" {
			ok++
		}
	}
	if ok == 0 {
		t.Fatalf("жодної придатної пари ПІБ+номер")
	}
	t.Logf("рядків %d, придатних пар %d", len(rows)-1, ok)
}
