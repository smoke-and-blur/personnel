package main

import (
	"strings"
	"testing"
)

// Дані у форматі «Підрозділ / Посада / Штат / в/з / ПІБ» після зіставлення колонок
// на клієнті: порожній «Штат» приходить як Extra=true.
func sample() []importRow {
	return []importRow{
		{Sheet: "Бойові", Unit: "1 взвод", Position: "начальник відділення", Tag: "МРТ", Rank: "штаб-сержант", PIB: "ІВАНЕНКО Іван Іванович"},
		{Sheet: "Бойові", Unit: "1 взвод", Position: "оператор FPV", Tag: "FPV", Rank: "солдат", PIB: " ПЕТРЕНКО Петро Петрович "},
		{Sheet: "Бойові", Unit: "1 взвод", Position: "оператор FPV", Tag: "FPV", Rank: "солдат", PIB: "СИДОРЕНКО Сидір Сидорович"},
		{Sheet: "Бойові", Unit: "1 взвод", Position: "оператор FPV", Tag: "FPV"}, // вакансія без людини
		{Sheet: "Бойові", Unit: "1 взвод", Position: "водій", Rank: "солдат", PIB: "КОВАЛЕНКО Микола Миколайович", Extra: true},
		{Sheet: "Бойові", Unit: "1 взвод", Position: "хтось", Extra: true},       // позаштатний без ПІБ — геть
		{Sheet: "Бойові", Unit: "", Position: "оператор МРТ", Tag: "МРТ"},        // без підрозділу — геть
		{Sheet: "Бойові", Unit: "2 взвод", Position: "", Tag: "МРТ", PIB: "ХТО"}, // без посади — геть
	}
}

func TestNormalizeImportRows(t *testing.T) {
	rows, skipped := normalizeImportRows(sample())
	if skipped != 3 {
		t.Errorf("пропущено = %d, очікували 3", skipped)
	}
	if len(rows) != 5 {
		t.Fatalf("лишилось рядків = %d, очікували 5", len(rows))
	}
	if rows[1].PIB != "ПЕТРЕНКО Петро Петрович" {
		t.Errorf("ПІБ не обрізано: %q", rows[1].PIB)
	}
}

func TestPlanSlotsCountsRepeatedPositions(t *testing.T) {
	rows, _ := normalizeImportRows(sample())
	keys, need, tagFor := planSlots(rows)

	// Позаштатні не створюють посад.
	if len(keys) != 2 {
		t.Fatalf("пар (підрозділ+посада) = %d, очікували 2: %v", len(keys), keys)
	}
	// Порядок появи у файлі має зберігатись — від нього залежить order у штатці.
	if got := keys[0]; got != slotKey("Бойові", "1 взвод", "начальник відділення", "МРТ") {
		t.Errorf("перша пара = %q", strings.ReplaceAll(got, "\x00", " / "))
	}
	// Три рядки «оператор FPV» = три окремі слоти, а не один.
	if n := need[slotKey("Бойові", "1 взвод", "оператор FPV", "FPV")]; n != 3 {
		t.Errorf("слотів на «оператор FPV» = %d, очікували 3", n)
	}
	if n := need[slotKey("Бойові", "1 взвод", "начальник відділення", "МРТ")]; n != 1 {
		t.Errorf("слотів на «начальник відділення» = %d, очікували 1", n)
	}
	if tg := tagFor[slotKey("Бойові", "1 взвод", "оператор FPV", "FPV")]; tg != "FPV" {
		t.Errorf("мітка = %q, очікували FPV", tg)
	}
	if _, ok := need[slotKey("Бойові", "1 взвод", "водій", "")]; ok {
		t.Error("позаштатний рядок не має створювати посаду")
	}
}

// Однакова посада у різних штатках — це різні слоти, вони не мають зливатись.
func TestPlanSlotsSeparatesSheets(t *testing.T) {
	rows, _ := normalizeImportRows([]importRow{
		{Sheet: "Бойові", Unit: "1 взвод", Position: "оператор", Tag: "МРТ"},
		{Sheet: "Охорона кордону", Unit: "1 взвод", Position: "оператор", Tag: "МРТ"},
	})
	keys, need, _ := planSlots(rows)
	if len(keys) != 2 {
		t.Fatalf("ключів = %d, очікували 2", len(keys))
	}
	for _, sh := range []string{"Бойові", "Охорона кордону"} {
		if n := need[slotKey(sh, "1 взвод", "оператор", "МРТ")]; n != 1 {
			t.Errorf("штатка %q: слотів = %d, очікували 1", sh, n)
		}
	}
}

// Повторний імпорт того самого файлу не повинен нічого дублювати:
// потреба в слотах не росте, а посади вже є.
func TestPlanSlotsIsStableOnReimport(t *testing.T) {
	rows, _ := normalizeImportRows(sample())
	_, need1, _ := planSlots(rows)
	_, need2, _ := planSlots(rows)
	for k, v := range need1 {
		if need2[k] != v {
			t.Errorf("потреба змінилась для %q: %d -> %d", k, v, need2[k])
		}
	}
}

func TestNormPIB(t *testing.T) {
	cases := [][2]string{
		{"  ІВАНЕНКО   Іван  Іванович ", "ІВАНЕНКО ІВАН ІВАНОВИЧ"},
		{"іваненко іван іванович", "ІВАНЕНКО ІВАН ІВАНОВИЧ"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normPIB(c[0]); got != c[1] {
			t.Errorf("normPIB(%q) = %q, очікували %q", c[0], got, c[1])
		}
	}
}

func TestSlotKeyIsUnambiguous(t *testing.T) {
	// Склейка не повинна плутати різні трійки з однаковим конкатенатом.
	if slotKey("", "а", "бв", "") == slotKey("", "аб", "в", "") {
		t.Error("ключі різних пар збіглися")
	}
	if slotKey("а", "б", "в", "") == slotKey("", "аб", "в", "") {
		t.Error("штатка не відокремлена від підрозділу")
	}
	// Напрям — частина ключа: та сама посада за FPV і за «Бомбером» це різні
	// штатні одиниці, інакше вони зіллються в одну й напрям загубиться.
	if slotKey("Ш", "п", "оператор", "FPV") == slotKey("Ш", "п", "оператор", "Бомбер") {
		t.Error("напрям не входить у ключ")
	}
	sheet, unit, position, tag := splitSlotKey(
		slotKey("Бойові", "1 взвод", "оператор FPV", "FPV"))
	if sheet != "Бойові" || unit != "1 взвод" || position != "оператор FPV" || tag != "FPV" {
		t.Errorf("розбір ключа: %q / %q / %q / %q", sheet, unit, position, tag)
	}
	// Порожня штатка і порожній напрям теж мають коректно розбиратись.
	if s, u, p, g := splitSlotKey(slotKey("", "2 взвод", "сапер", "")); s != "" ||
		u != "2 взвод" || p != "сапер" || g != "" {
		t.Errorf("розбір без штатки: %q / %q / %q / %q", s, u, p, g)
	}
}

// Одна посада в одному підрозділі, але за різними напрямами — це різні слоти.
// Раніше вони зливались, і всі копії діставали мітку першого рядка.
func TestPlanSlotsSplitsByTag(t *testing.T) {
	rows, _ := normalizeImportRows([]importRow{
		{Sheet: "Ш", Unit: "гр", Position: "оператор", Tag: "FPV", PIB: "А А А"},
		{Sheet: "Ш", Unit: "гр", Position: "оператор", Tag: "Бомбер", PIB: "Б Б Б"},
		{Sheet: "Ш", Unit: "гр", Position: "оператор", Tag: "FPV", PIB: "В В В"},
	})
	keys, need, tagFor := planSlots(rows)
	if len(keys) != 2 {
		t.Fatalf("слотів = %d, очікували 2 (FPV і Бомбер)", len(keys))
	}
	if n := need[slotKey("Ш", "гр", "оператор", "FPV")]; n != 2 {
		t.Errorf("FPV: %d, очікували 2", n)
	}
	if n := need[slotKey("Ш", "гр", "оператор", "Бомбер")]; n != 1 {
		t.Errorf("Бомбер: %d, очікували 1", n)
	}
	if tg := tagFor[slotKey("Ш", "гр", "оператор", "Бомбер")]; tg != "Бомбер" {
		t.Errorf("мітка = %q, очікували Бомбер", tg)
	}
}
