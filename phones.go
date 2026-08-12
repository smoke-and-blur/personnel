package main

// Разове доливання телефонів зі стороннього списку.
//
//	personnel -phones "Учасники.xlsx"          — лише показати, що зміниться
//	personnel -phones "Учасники.xlsx" -apply   — записати
//
// Єдиний спільний ключ між системами — ПІБ, тож зводимо його до літер у
// нижньому регістрі. Якщо в складі двоє з однаковим ПІБ — пропускаємо: тут
// краще нічого не зробити, ніж записати навмання.

import (
	"archive/zip"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

var nonLetter = regexp.MustCompile(`[^\p{L}\p{N}]+`)

func pibKey(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\'', '’', '`', 'ʼ':
			return -1
		}
		return unicode.ToLower(r)
	}, s)
	return strings.TrimSpace(nonLetter.ReplaceAllString(s, " "))
}

// Номер зводимо до +380XXXXXXXXX; те, що на телефон не схоже, відкидаємо.
func normPhone(s string) string {
	var d strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			d.WriteRune(r)
		}
	}
	v := d.String()
	switch {
	case len(v) == 12 && strings.HasPrefix(v, "380"):
		return "+" + v
	case len(v) == 10 && strings.HasPrefix(v, "0"):
		return "+38" + v
	case len(v) == 9:
		return "+380" + v
	}
	return ""
}

// ---------- читання xlsx без сторонніх бібліотек: це zip з XML ----------

type xlsxRow struct {
	Cells []struct {
		Ref  string `xml:"r,attr"`
		Type string `xml:"t,attr"`
		V    string `xml:"v"`
		IS   struct {
			T string `xml:"t"`
		} `xml:"is"`
	} `xml:"c"`
}

func colIndex(ref string) int {
	n := 0
	for _, r := range ref {
		if r < 'A' || r > 'Z' {
			break
		}
		n = n*26 + int(r-'A') + 1
	}
	return n - 1
}

func readXLSX(path string) ([][]string, error) {
	z, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer z.Close()

	var shared []string
	var sheet []byte
	for _, f := range z.File {
		switch {
		case f.Name == "xl/sharedStrings.xml":
			b, err := readAll(f)
			if err != nil {
				return nil, err
			}
			var ss struct {
				SI []struct {
					T string   `xml:"t"`
					R []string `xml:"r>t"`
				} `xml:"si"`
			}
			if err := xml.Unmarshal(b, &ss); err != nil {
				return nil, err
			}
			for _, si := range ss.SI {
				if len(si.R) > 0 {
					shared = append(shared, strings.Join(si.R, ""))
				} else {
					shared = append(shared, si.T)
				}
			}
		case f.Name == "xl/worksheets/sheet1.xml":
			b, err := readAll(f)
			if err != nil {
				return nil, err
			}
			sheet = b
		}
	}
	if sheet == nil {
		return nil, fmt.Errorf("у файлі немає аркуша")
	}
	var ws struct {
		Rows []xlsxRow `xml:"sheetData>row"`
	}
	if err := xml.Unmarshal(sheet, &ws); err != nil {
		return nil, err
	}
	out := make([][]string, 0, len(ws.Rows))
	for _, r := range ws.Rows {
		var row []string
		for _, c := range r.Cells {
			i := colIndex(c.Ref)
			for len(row) <= i {
				row = append(row, "")
			}
			switch c.Type {
			case "s":
				if n, err := strconv.Atoi(c.V); err == nil && n < len(shared) {
					row[i] = shared[n]
				}
			case "inlineStr":
				row[i] = c.IS.T
			default:
				row[i] = c.V
			}
		}
		out = append(out, row)
	}
	return out, nil
}

func readAll(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// ---------- зіставлення й запис ----------

func cell(row []string, i int) string {
	if i < 0 || i >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[i])
}

// Колонки шукаємо за заголовком; «основного» має перевагу над просто «телефон».
func pickColumns(head []string) (pib, phone int) {
	pib, phone = -1, -1
	for i, h := range head {
		l := strings.ToLower(h)
		if pib < 0 && (strings.Contains(l, "піб") || strings.Contains(l, "прізвище")) {
			pib = i
		}
		if strings.Contains(l, "основного") {
			phone = i
		} else if phone < 0 && (strings.Contains(l, "телефон") || strings.Contains(l, "номер")) {
			phone = i
		}
	}
	return
}

func runPhones(ctx context.Context, path string, apply bool) {
	rows, err := readXLSX(path)
	if err != nil {
		fatalf("не вдалося прочитати %s: %v", path, err)
	}
	if len(rows) < 2 {
		fatalf("у файлі немає даних")
	}
	ip, it := pickColumns(rows[0])
	if ip < 0 || it < 0 {
		fatalf("не знайшов колонок ПІБ і телефону в заголовку")
	}

	// Хто зараз у складі. Однакові ПІБ позначаємо як неоднозначні.
	cur, err := persons.Find(ctx, bson.M{"status": "active"})
	if err != nil {
		fatalf("%v", err)
	}
	var list []Person
	if err := cur.All(ctx, &list); err != nil {
		fatalf("%v", err)
	}
	byKey := map[string]*Person{}
	ambiguous := map[string]bool{}
	for i := range list {
		k := pibKey(list[i].PIB)
		if _, seen := byKey[k]; seen {
			ambiguous[k] = true
			continue
		}
		byKey[k] = &list[i]
	}

	var plan []struct {
		ID    primitive.ObjectID
		PIB   string
		From  string
		Phone string
	}
	var same, dup, miss int
	for _, r := range rows[1:] {
		name, phone := cell(r, ip), normPhone(cell(r, it))
		if name == "" || phone == "" {
			continue
		}
		k := pibKey(name)
		if ambiguous[k] {
			dup++
			continue
		}
		p := byKey[k]
		if p == nil {
			miss++
			continue
		}
		if p.Phone == phone {
			same++
			continue
		}
		if p.Phone != "" { // наявний номер не чіпаємо
			same++
			continue
		}
		plan = append(plan, struct {
			ID    primitive.ObjectID
			PIB   string
			From  string
			Phone string
		}{p.ID, p.PIB, p.Phone, phone})
	}

	fmt.Printf("у складі %d · у файлі %d · допишемо %d · без змін %d · однакових ПІБ %d · немає в складі %d\n",
		len(list), len(rows)-1, len(plan), same, dup, miss)
	if !apply {
		for i, x := range plan {
			if i == 15 {
				fmt.Printf("  … і ще %d\n", len(plan)-15)
				break
			}
			fmt.Printf("  %s → %s\n", x.PIB, x.Phone)
		}
		fmt.Println("це лише перегляд; додайте -apply, щоб записати")
		return
	}
	now := time.Now().UTC()
	done := 0
	for _, x := range plan {
		_, err := persons.UpdateByID(ctx, x.ID,
			bson.M{"$set": bson.M{"phone": x.Phone, "updatedAt": now}})
		if err != nil {
			fmt.Printf("  %s: %v\n", x.PIB, err)
			continue
		}
		done++
	}
	addLog(ctx, "", "—", "Телефони", fmt.Sprintf("залито зі списку, записів: %d", done))
	fmt.Printf("записано номерів: %d\n", done)
}

// ---------- разове перейменування підрозділів ----------
//
//	personnel -units units.tsv          — показати, що зміниться
//	personnel -units units.tsv -apply   — записати
//
// Файл: дві колонки через табуляцію — стара назва і нова. Тримаємо його поза
// репозиторієм: справжні назви підрозділів там зберігати не можна.
// Перелік наявних значень поля — щоб було з чого будувати карту скорочень.
func runDump(ctx context.Context, field string) {
	vals, err := posts.Distinct(ctx, field, bson.M{})
	if err != nil {
		fatalf("%v", err)
	}
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if str, ok := v.(string); ok && str != "" {
			out = append(out, str)
		}
	}
	sort.Strings(out)
	for _, v := range out {
		fmt.Println(v)
	}
	fmt.Fprintf(os.Stderr, "усього: %d\n", len(out))
}

func runUnits(ctx context.Context, path string, apply bool) {
	runRename(ctx, "unit", path, apply)
}

func runPositions(ctx context.Context, path string, apply bool) {
	runRename(ctx, "position", path, apply)
}

func runRename(ctx context.Context, field, path string, apply bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		fatalf("не вдалося прочитати %s: %v", path, err)
	}
	ren := map[string]string{}
	for i, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			fatalf("рядок %d: очікую дві колонки через табуляцію", i+1)
		}
		from, to := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if from == "" || to == "" {
			fatalf("рядок %d: порожня назва", i+1)
		}
		ren[from] = to
	}
	if len(ren) == 0 {
		fatalf("у файлі немає жодної пари")
	}

	type plan struct {
		from, to      string
		posts, people int64
	}
	var out []plan
	for from, to := range ren {
		np, err := posts.CountDocuments(ctx, bson.M{field: from})
		if err != nil {
			fatalf("%v", err)
		}
		// У штатних unit — копія з посади, у позаштатних — власне поле.
		nu, err := persons.CountDocuments(ctx, bson.M{field: from})
		if err != nil {
			fatalf("%v", err)
		}
		out = append(out, plan{from, to, np, nu})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].from < out[j].from })

	var tp, tu int64
	for _, p := range out {
		fmt.Printf("  %-4d посад %-4d людей   %s → %s\n", p.posts, p.people, p.from, p.to)
		tp += p.posts
		tu += p.people
	}
	fmt.Printf("разом: пар %d · посад %d · людей %d\n", len(out), tp, tu)
	if !apply {
		fmt.Println("це лише перегляд; додайте -apply, щоб записати")
		return
	}
	now := time.Now().UTC()
	for _, p := range out {
		if _, err := posts.UpdateMany(ctx, bson.M{field: p.from},
			bson.M{"$set": bson.M{field: p.to}}); err != nil {
			fmt.Printf("  %s: %v\n", p.from, err)
			continue
		}
		if _, err := persons.UpdateMany(ctx, bson.M{field: p.from},
			bson.M{"$set": bson.M{field: p.to, "updatedAt": now}}); err != nil {
			fmt.Printf("  %s: %v\n", p.from, err)
		}
	}
	addLog(ctx, "", "—", "Перейменування", fmt.Sprintf("поле %s, назв: %d", field, len(out)))
	fmt.Printf("перейменовано: %d\n", len(out))
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
