package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

//go:embed static
var staticFS embed.FS

// Захардкожений ключ доступу. Можна перекрити змінною середовища API_KEY.
const hardcodedKey = "secret"

var (
	apiKey  = hardcodedKey
	posts   *mongo.Collection
	persons *mongo.Collection
	journal *mongo.Collection
	imports *mongo.Collection
)

// ---------- Моделі ----------

// Post — штатна посада. Назва і мітка задаються імпортом і не редагуються.
// Sheet — назва штатки: їх може бути кілька (бойові, охорона кордону тощо),
// звіти й підсумки рахуються за кожною окремо. Порожня = «Основна».
type Post struct {
	ID       primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	Batch    string             `bson:"batch,omitempty" json:"batch"`
	Sheet    string             `bson:"sheet" json:"sheet"`
	Unit     string             `bson:"unit" json:"unit"`
	Position string             `bson:"position" json:"position"`
	Tag      string             `bson:"tag" json:"tag"`
	Order    int                `bson:"order" json:"order"`
}

type Cert struct {
	ID     string `bson:"id" json:"id"`
	Tag    string `bson:"tag" json:"tag"`
	Level  int    `bson:"level" json:"level"`
	Date   string `bson:"date" json:"date"`
	School string `bson:"school" json:"school"`
	Course string `bson:"course" json:"course"`
}

// Absence — відсутність: хв (хворий), вдр (відрядження), вдп (відпустка) тощо.
// From/To — дати «з» і «по» у форматі YYYY-MM-DD; порожнє To = без визначеної дати.
type Absence struct {
	ID    string `bson:"id" json:"id"`
	Kind  string `bson:"kind" json:"kind"`
	From  string `bson:"from" json:"from"`
	To    string `bson:"to" json:"to"`
	Place string `bson:"place" json:"place"`
	Note  string `bson:"note" json:"note"`
}

// Person: PostID != "" — штатний (unit/position/positionTag — копія з посади),
// PostID == "" — позаштатний (position — вільний текст, positionTag порожня).
type Person struct {
	ID          primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	PIB         string             `bson:"pib" json:"pib"`
	Rank        string             `bson:"rank" json:"rank"`
	PostID      string             `bson:"postId" json:"postId"`
	Unit        string             `bson:"unit" json:"unit"`
	Position    string             `bson:"position" json:"position"`
	PositionTag string             `bson:"positionTag" json:"positionTag"`
	Phone       string             `bson:"phone" json:"phone"`
	Status      string             `bson:"status" json:"status"` // active | dismissed
	Note        string             `bson:"note" json:"note"`
	Batch       string             `bson:"batch,omitempty" json:"batch"`
	Certs       []Cert             `bson:"certs" json:"certs"`
	Absences    []Absence          `bson:"absences" json:"absences"`
	CreatedAt   time.Time          `bson:"createdAt" json:"createdAt"`
	UpdatedAt   time.Time          `bson:"updatedAt" json:"updatedAt"`
}

// ImportBatch — слід одного імпорту, щоб його можна було скасувати.
type ImportBatch struct {
	ID     primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	TS     time.Time          `bson:"ts" json:"ts"`
	Sheets []string           `bson:"sheets" json:"sheets"`
	Posts  int                `bson:"posts" json:"posts"`
	Staff  int                `bson:"staff" json:"staff"`
	Extras int                `bson:"extras" json:"extras"`
}

type LogEntry struct {
	ID       primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	PersonID string             `bson:"personId" json:"personId"`
	PIB      string             `bson:"pib" json:"pib"`
	Action   string             `bson:"action" json:"action"`
	Details  string             `bson:"details" json:"details"`
	TS       time.Time          `bson:"ts" json:"ts"`
}

// ---------- Допоміжне ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func addLog(ctx context.Context, personID, pib, action, details string) {
	if _, err := journal.InsertOne(ctx, LogEntry{
		PersonID: personID, PIB: pib, Action: action, Details: details, TS: time.Now().UTC(),
	}); err != nil {
		log.Printf("journal insert: %v", err)
	}
}

func getPerson(ctx context.Context, idHex string) (*Person, error) {
	oid, err := primitive.ObjectIDFromHex(idHex)
	if err != nil {
		return nil, errors.New("некоректний id")
	}
	var p Person
	if err := persons.FindOne(ctx, bson.M{"_id": oid}).Decode(&p); err != nil {
		return nil, errors.New("людину не знайдено")
	}
	return &p, nil
}

func getPost(ctx context.Context, idHex string) (*Post, error) {
	oid, err := primitive.ObjectIDFromHex(idHex)
	if err != nil {
		return nil, errors.New("некоректний id посади")
	}
	var p Post
	if err := posts.FindOne(ctx, bson.M{"_id": oid}).Decode(&p); err != nil {
		return nil, errors.New("посаду не знайдено")
	}
	return &p, nil
}

// Посада зайнята, якщо на ній є активна людина.
func postOccupied(ctx context.Context, postID string, exceptPerson primitive.ObjectID) (bool, error) {
	f := bson.M{"postId": postID, "status": "active"}
	if !exceptPerson.IsZero() {
		f["_id"] = bson.M{"$ne": exceptPerson}
	}
	n, err := persons.CountDocuments(ctx, f)
	return n > 0, err
}

func auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != apiKey {
			writeErr(w, http.StatusUnauthorized, "невірний ключ доступу")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- Штатка ----------

func listPosts(w http.ResponseWriter, r *http.Request) {
	cur, err := posts.Find(r.Context(), bson.M{},
		options.Find().SetSort(bson.D{{Key: "sheet", Value: 1}, {Key: "order", Value: 1},
			{Key: "unit", Value: 1}, {Key: "position", Value: 1}}))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := []Post{}
	if err := cur.All(r.Context(), &out); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, out)
}

// Видалити можна лише вакантну посаду.
func deletePost(w http.ResponseWriter, r *http.Request) {
	p, err := getPost(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	occ, err := postOccupied(r.Context(), p.ID.Hex(), primitive.NilObjectID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if occ {
		writeErr(w, 400, "посада зайнята — спочатку звільніть її")
		return
	}
	if _, err := posts.DeleteOne(r.Context(), bson.M{"_id": p.ID}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---------- Персонал ----------

func listPersons(w http.ResponseWriter, r *http.Request) {
	cur, err := persons.Find(r.Context(), bson.M{},
		options.Find().SetSort(bson.D{{Key: "unit", Value: 1}, {Key: "pib", Value: 1}}))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := []Person{}
	if err := cur.All(r.Context(), &out); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, out)
}

// Створення: якщо postId задано — людина сідає на штатну посаду (має бути вакантна),
// інакше — позаштатний з вільним текстом посади.
func createPerson(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PIB      string `json:"pib"`
		Rank     string `json:"rank"`
		PostID   string `json:"postId"`
		Unit     string `json:"unit"`
		Position string `json:"position"`
		Note     string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, "некоректний JSON")
		return
	}
	in.PIB = strings.TrimSpace(in.PIB)
	if in.PIB == "" {
		writeErr(w, 400, "ПІБ обовʼязкове")
		return
	}
	ctx := r.Context()
	now := time.Now().UTC()
	p := Person{
		PIB: in.PIB, Rank: strings.TrimSpace(in.Rank), Note: in.Note,
		Status: "active", Certs: []Cert{}, Absences: []Absence{}, CreatedAt: now, UpdatedAt: now,
	}
	var logDetails string
	if in.PostID != "" {
		post, err := getPost(ctx, in.PostID)
		if err != nil {
			writeErr(w, 404, err.Error())
			return
		}
		occ, err := postOccupied(ctx, in.PostID, primitive.NilObjectID)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if occ {
			writeErr(w, 409, "посада вже зайнята")
			return
		}
		p.PostID = in.PostID
		p.Unit, p.Position, p.PositionTag = post.Unit, post.Position, post.Tag
		logDetails = fmt.Sprintf("штатний: %s, %s", post.Unit, post.Position)
	} else {
		p.Unit = strings.TrimSpace(in.Unit)
		p.Position = strings.TrimSpace(in.Position)
		logDetails = fmt.Sprintf("позаштатний: %s, %s", p.Unit, p.Position)
	}
	res, err := persons.InsertOne(ctx, p)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	p.ID = res.InsertedID.(primitive.ObjectID)
	addLog(ctx, p.ID.Hex(), p.PIB, "Додано", logDetails)
	writeJSON(w, 201, p)
}

// Часткове (інлайн) оновлення: передаються лише змінені поля.
// Для штатних unit/position змінюються тільки через переміщення.
func updatePerson(w http.ResponseWriter, r *http.Request) {
	p, err := getPerson(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	var in struct {
		PIB      *string `json:"pib"`
		Rank     *string `json:"rank"`
		Unit     *string `json:"unit"`
		Position *string `json:"position"`
		Phone    *string `json:"phone"`
		Note     *string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, "некоректний JSON")
		return
	}
	if p.PostID != "" && (in.Unit != nil || in.Position != nil) {
		writeErr(w, 400, "у штатних підрозділ і посада змінюються лише через переміщення")
		return
	}
	set := bson.M{"updatedAt": time.Now().UTC()}
	changes := []string{}
	chg := func(name, oldV, newV, field string) {
		if oldV != newV {
			set[field] = newV
			changes = append(changes, fmt.Sprintf("%s: «%s» → «%s»", name, oldV, newV))
		}
	}
	if in.PIB != nil {
		v := strings.TrimSpace(*in.PIB)
		if v == "" {
			writeErr(w, 400, "ПІБ не може бути порожнім")
			return
		}
		chg("ПІБ", p.PIB, v, "pib")
	}
	if in.Rank != nil {
		chg("звання", p.Rank, strings.TrimSpace(*in.Rank), "rank")
	}
	if in.Unit != nil {
		chg("підрозділ", p.Unit, strings.TrimSpace(*in.Unit), "unit")
	}
	if in.Position != nil {
		chg("посада", p.Position, strings.TrimSpace(*in.Position), "position")
	}
	if in.Phone != nil {
		chg("телефон", p.Phone, strings.TrimSpace(*in.Phone), "phone")
	}
	if in.Note != nil {
		chg("примітка", p.Note, *in.Note, "note")
	}
	if len(changes) == 0 {
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	if _, err := persons.UpdateByID(r.Context(), p.ID, bson.M{"$set": set}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	pib := p.PIB
	if v, ok := set["pib"].(string); ok {
		pib = v
	}
	addLog(r.Context(), p.ID.Hex(), pib, "Редаговано", strings.Join(changes, "; "))
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// Переміщення/призначення на штатну посаду або зняття в позаштатні.
func movePerson(w http.ResponseWriter, r *http.Request) {
	p, err := getPerson(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	if p.Status != "active" {
		writeErr(w, 400, "людина не в активному складі")
		return
	}
	var in struct {
		PostID string `json:"postId"` // цільова посада
		Extra  bool   `json:"extra"`  // true = зняти в позаштатні
		Reason string `json:"reason"`
		// Що робити з тим, хто вже сидить на цільовій посаді:
		// swap — на посаду того, хто переміщується; extra — у позаштатні.
		// Порожнє — зайняту посаду не чіпаємо (409).
		Displace string `json:"displace"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, "некоректний JSON")
		return
	}
	ctx := r.Context()
	from := fmt.Sprintf("%s, %s", p.Unit, p.Position)
	set := bson.M{"updatedAt": time.Now().UTC()}
	var action, details string

	switch {
	case in.Extra:
		if p.PostID == "" {
			writeErr(w, 400, "людина вже позаштатна")
			return
		}
		set["postId"] = ""
		set["positionTag"] = ""
		action = "У позаштатні"
		details = "з посади: " + from
	case in.PostID != "":
		if in.PostID == p.PostID {
			writeErr(w, 400, "людина вже на цій посаді")
			return
		}
		post, err := getPost(ctx, in.PostID)
		if err != nil {
			writeErr(w, 404, err.Error())
			return
		}
		// Хто зараз сидить на цільовій посаді (якщо взагалі сидить).
		var cur Person
		occupied := persons.FindOne(ctx, bson.M{
			"postId": in.PostID, "status": "active", "_id": bson.M{"$ne": p.ID},
		}).Decode(&cur) == nil
		if occupied {
			curSet := bson.M{"updatedAt": time.Now().UTC()}
			var curAction, curDetails string
			switch in.Displace {
			case "swap":
				if p.PostID == "" {
					writeErr(w, 400, "обмін неможливий: у того, кого переміщуємо, немає посади")
					return
				}
				curSet["postId"] = p.PostID
				curSet["unit"] = p.Unit
				curSet["position"] = p.Position
				curSet["positionTag"] = p.PositionTag
				curAction = "Переміщено"
				curDetails = fmt.Sprintf("обмін з %s: %s, %s → %s, %s",
					p.PIB, post.Unit, post.Position, p.Unit, p.Position)
			case "extra":
				curSet["postId"] = ""
				curSet["positionTag"] = ""
				curAction = "У позаштатні"
				curDetails = fmt.Sprintf("посаду %s, %s зайняв %s", post.Unit, post.Position, p.PIB)
			default:
				writeErr(w, 409, "цільова посада зайнята — вкажіть, куди подіти "+cur.PIB)
				return
			}
			if in.Reason != "" {
				curDetails += "; підстава: " + in.Reason
			}
			if _, err := persons.UpdateByID(ctx, cur.ID, bson.M{"$set": curSet}); err != nil {
				writeErr(w, 500, err.Error())
				return
			}
			addLog(ctx, cur.ID.Hex(), cur.PIB, curAction, curDetails)
		}
		set["postId"] = in.PostID
		set["unit"] = post.Unit
		set["position"] = post.Position
		set["positionTag"] = post.Tag
		if p.PostID == "" {
			action = "Призначено"
			details = fmt.Sprintf("позаштатний (%s) → %s, %s", from, post.Unit, post.Position)
		} else {
			action = "Переміщено"
			details = fmt.Sprintf("%s → %s, %s", from, post.Unit, post.Position)
		}
		if occupied {
			details += fmt.Sprintf(" (посаду звільнив %s)", cur.PIB)
		}
	default:
		writeErr(w, 400, "вкажіть цільову посаду або extra")
		return
	}
	if in.Reason != "" {
		details += "; підстава: " + in.Reason
	}
	if _, err := persons.UpdateByID(ctx, p.ID, bson.M{"$set": set}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	addLog(ctx, p.ID.Hex(), p.PIB, action, details)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func dismissPerson(w http.ResponseWriter, r *http.Request) {
	p, err := getPerson(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	// postId лишається в записі (історія), але посада стає вакантною,
	// бо зайнятість рахується лише по активних.
	if _, err := persons.UpdateByID(r.Context(), p.ID, bson.M{"$set": bson.M{
		"status": "dismissed", "updatedAt": time.Now().UTC(),
	}}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	details := fmt.Sprintf("був: %s, %s", p.Unit, p.Position)
	if in.Reason != "" {
		details = in.Reason + "; " + details
	}
	addLog(r.Context(), p.ID.Hex(), p.PIB, "Вибув", details)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// Поновлення: якщо стара посада досі вакантна — повертається на неї, інакше — у позаштатні.
func restorePerson(w http.ResponseWriter, r *http.Request) {
	p, err := getPerson(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	ctx := r.Context()
	set := bson.M{"status": "active", "updatedAt": time.Now().UTC()}
	details := "повернуто в активний склад"
	if p.PostID != "" {
		occ, err := postOccupied(ctx, p.PostID, p.ID)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if occ {
			set["postId"] = ""
			set["positionTag"] = ""
			details += " (посада вже зайнята — у позаштатні)"
		} else {
			details += fmt.Sprintf(" на посаду: %s, %s", p.Unit, p.Position)
		}
	} else {
		details += " (позаштатний)"
	}
	if _, err := persons.UpdateByID(ctx, p.ID, bson.M{"$set": set}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	addLog(ctx, p.ID.Hex(), p.PIB, "Поновлено", details)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func deletePerson(w http.ResponseWriter, r *http.Request) {
	p, err := getPerson(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	if p.Status != "dismissed" {
		writeErr(w, 400, "спочатку позначте людину як «вибув»")
		return
	}
	if _, err := persons.DeleteOne(r.Context(), bson.M{"_id": p.ID}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	addLog(r.Context(), p.ID.Hex(), p.PIB, "Видалено",
		fmt.Sprintf("Запис видалено остаточно; був: %s, %s (журнал збережено)", p.Unit, p.Position))
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---------- Сертифікати ----------

func addCert(w http.ResponseWriter, r *http.Request) {
	p, err := getPerson(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	var c Cert
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeErr(w, 400, "некоректний JSON")
		return
	}
	if c.Tag == "" {
		writeErr(w, 400, "мітка сертифіката обовʼязкова")
		return
	}
	if c.Level < 1 {
		c.Level = 1
	}
	c.ID = primitive.NewObjectID().Hex()
	if _, err := persons.UpdateByID(r.Context(), p.ID, bson.M{
		"$push": bson.M{"certs": c},
		"$set":  bson.M{"updatedAt": time.Now().UTC()},
	}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	addLog(r.Context(), p.ID.Hex(), p.PIB, "Додано допуск",
		fmt.Sprintf("%s р.%d, %s (%s)", c.Tag, c.Level, c.Course, c.School))
	writeJSON(w, 201, c)
}

func deleteCert(w http.ResponseWriter, r *http.Request) {
	p, err := getPerson(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	certID := r.PathValue("certId")
	var removed *Cert
	for i := range p.Certs {
		if p.Certs[i].ID == certID {
			removed = &p.Certs[i]
			break
		}
	}
	if removed == nil {
		writeErr(w, 404, "сертифікат не знайдено")
		return
	}
	if _, err := persons.UpdateByID(r.Context(), p.ID, bson.M{
		"$pull": bson.M{"certs": bson.M{"id": certID}},
		"$set":  bson.M{"updatedAt": time.Now().UTC()},
	}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	addLog(r.Context(), p.ID.Hex(), p.PIB, "Знято допуск", fmt.Sprintf("%s р.%d", removed.Tag, removed.Level))
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---------- Імпорт ----------

// importRow — рядок вхідної таблиці. Extra=true (у файлі не вказано «Штат»)
// означає лише позаштатну людину, посада в штатці не створюється.
type importRow struct {
	Sheet    string `json:"sheet"`
	Unit     string `json:"unit"`
	Position string `json:"position"`
	Tag      string `json:"tag"`
	Rank     string `json:"rank"`
	PIB      string `json:"pib"`
	Phone    string `json:"phone"`
	Extra    bool   `json:"extra"`
}

type importResult struct {
	Batch   string `json:"batch"`   // мітка партії — за нею скасовують імпорт
	Posts   int    `json:"posts"`   // створено посад
	Tagged  int    `json:"tagged"`  // наявним посадам проставлено мітку
	Staff   int    `json:"staff"`   // додано штатних людей
	Extras  int    `json:"extras"`  // додано позаштатних
	Skipped int    `json:"skipped"` // пропущено рядків
}

func normPIB(s string) string { return strings.ToUpper(strings.Join(strings.Fields(s), " ")) }

// Слот однозначно визначається четвіркою штатка+підрозділ+посада+напрям.
// Напрям тут не зайвий: та сама посада в одному підрозділі буває і за FPV,
// і за «Бомбером» — це різні штатні одиниці. Без нього вони зливались в одну
// пару, і кожна діставала мітку першого рядка у файлі.
func slotKey(sheet, unit, position, tag string) string {
	return sheet + "\x00" + unit + "\x00" + position + "\x00" + tag
}

func splitSlotKey(k string) (sheet, unit, position, tag string) {
	parts := strings.SplitN(k, "\x00", 4)
	for len(parts) < 4 {
		parts = append(parts, "")
	}
	return parts[0], parts[1], parts[2], parts[3]
}

// Чистка пробілів і відсів непридатних рядків.
func normalizeImportRows(in []importRow) (rows []importRow, skipped int) {
	rows = make([]importRow, 0, len(in))
	for _, row := range in {
		row.Sheet = strings.TrimSpace(row.Sheet)
		row.Unit = strings.TrimSpace(row.Unit)
		row.Position = strings.TrimSpace(row.Position)
		row.Tag = strings.TrimSpace(row.Tag)
		row.Rank = strings.TrimSpace(row.Rank)
		row.PIB = strings.TrimSpace(row.PIB)
		row.Phone = strings.TrimSpace(row.Phone)
		if row.Extra && row.PIB == "" {
			skipped++ // позаштатний без ПІБ — порожній рядок
			continue
		}
		if !row.Extra && (row.Unit == "" || row.Position == "") {
			skipped++
			continue
		}
		rows = append(rows, row)
	}
	return rows, skipped
}

// Скільки слотів треба на кожну трійку (штатка+підрозділ+посада) — у порядку появи
// у файлі, щоб нові посади лягли у штатку тим самим порядком, що й у джерелі.
func planSlots(rows []importRow) (keys []string, need map[string]int, tagFor map[string]string) {
	need, tagFor = map[string]int{}, map[string]string{}
	for _, row := range rows {
		if row.Extra {
			continue
		}
		k := slotKey(row.Sheet, row.Unit, row.Position, row.Tag)
		if _, ok := need[k]; !ok {
			keys = append(keys, k)
		}
		need[k]++
		if tagFor[k] == "" {
			tagFor[k] = row.Tag
		}
	}
	return keys, need, tagFor
}

// Імпорт штатки разом з людьми. Скільки однакових пар (підрозділ+посада) у файлі —
// стільки й слотів у штатці; наявні посади перевикористовуються, тож повторний
// імпорт того самого файлу нічого не дублює. Людей звіряємо за ПІБ.
func importData(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Rows []importRow `json:"rows"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, "некоректний JSON")
		return
	}
	ctx := r.Context()
	var res importResult
	batch := primitive.NewObjectID()
	res.Batch = batch.Hex()

	rows, skipped := normalizeImportRows(in.Rows)
	res.Skipped = skipped

	// Поточний стан: зайняті посади і вже відомі ПІБ.
	cur, err := persons.Find(ctx, bson.M{})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var existing []Person
	if err := cur.All(ctx, &existing); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	occupied := map[string]bool{}
	seenPIB := map[string]bool{}
	for _, p := range existing {
		if p.Status == "active" && p.PostID != "" {
			occupied[p.PostID] = true
		}
		if p.PIB != "" {
			seenPIB[normPIB(p.PIB)] = true
		}
	}

	var maxOrder struct {
		Order int `bson:"order"`
	}
	_ = posts.FindOne(ctx, bson.M{}, options.FindOne().SetSort(bson.D{{Key: "order", Value: -1}})).Decode(&maxOrder)
	order := maxOrder.Order

	// 1) Скільки слотів треба на кожну пару (у порядку появи у файлі).
	keyOrder, need, tagFor := planSlots(rows)

	// 2) Довести кількість посад до потрібної.
	slots := map[string][]Post{}
	for _, k := range keyOrder {
		sheet, unit, position, tag := splitSlotKey(k)
		// Посади без мітки теж підходять — нижче їм проставлять мітку з файлу.
		filter := bson.M{"sheet": sheet, "unit": unit, "position": position}
		if tag != "" {
			filter["tag"] = bson.M{"$in": []string{tag, ""}}
		} else {
			filter["tag"] = ""
		}
		c, err := posts.Find(ctx, filter,
			options.Find().SetSort(bson.D{{Key: "order", Value: 1}}))
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		var have []Post
		if err := c.All(ctx, &have); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		// Наявним посадам без мітки проставити мітку з файлу.
		if tagFor[k] != "" {
			for i := range have {
				if have[i].Tag != "" {
					continue
				}
				if _, err := posts.UpdateByID(ctx, have[i].ID, bson.M{"$set": bson.M{"tag": tagFor[k]}}); err != nil {
					writeErr(w, 500, err.Error())
					return
				}
				have[i].Tag = tagFor[k]
				res.Tagged++
			}
		}
		for len(have) < need[k] {
			order++
			p := Post{Batch: res.Batch, Sheet: sheet, Unit: unit, Position: position,
				Tag: tagFor[k], Order: order}
			ins, err := posts.InsertOne(ctx, p)
			if err != nil {
				writeErr(w, 500, err.Error())
				return
			}
			p.ID = ins.InsertedID.(primitive.ObjectID)
			have = append(have, p)
			res.Posts++
		}
		slots[k] = have
	}

	// 3) Люди: штатні сідають у перший вільний слот своєї пари.
	now := time.Now().UTC()
	for _, row := range rows {
		if row.PIB == "" {
			continue
		}
		np := normPIB(row.PIB)
		if seenPIB[np] {
			res.Skipped++ // така людина вже є
			continue
		}
		p := Person{
			Batch: res.Batch, PIB: row.PIB, Rank: row.Rank, Phone: row.Phone, Status: "active",
			Certs: []Cert{}, Absences: []Absence{}, CreatedAt: now, UpdatedAt: now,
		}
		if row.Extra {
			p.Unit, p.Position = row.Unit, row.Position
		} else {
			k := slotKey(row.Sheet, row.Unit, row.Position, row.Tag)
			var target *Post
			for i := range slots[k] {
				if !occupied[slots[k][i].ID.Hex()] {
					target = &slots[k][i]
					break
				}
			}
			if target == nil {
				res.Skipped++ // вільних слотів на цю посаду не лишилось
				continue
			}
			occupied[target.ID.Hex()] = true
			p.PostID = target.ID.Hex()
			p.Unit, p.Position, p.PositionTag = target.Unit, target.Position, target.Tag
		}
		if _, err := persons.InsertOne(ctx, p); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		seenPIB[np] = true
		if row.Extra {
			res.Extras++
		} else {
			res.Staff++
		}
	}

	// Один підсумковий запис замість сотні рядків на кожну людину.
	if res.Posts+res.Tagged+res.Staff+res.Extras > 0 {
		seen := map[string]bool{}
		sheets := []string{}
		for _, row := range rows {
			if !row.Extra && !seen[row.Sheet] {
				seen[row.Sheet] = true
				sheets = append(sheets, row.Sheet)
			}
		}
		if _, err := imports.InsertOne(ctx, ImportBatch{
			ID: batch, TS: time.Now().UTC(), Sheets: sheets,
			Posts: res.Posts, Staff: res.Staff, Extras: res.Extras,
		}); err != nil {
			log.Printf("import batch: %v", err)
		}
		addLog(ctx, "", "—", "Імпорт", fmt.Sprintf("посад: %d, штатних: %d, позаштатних: %d, пропущено: %d",
			res.Posts, res.Staff, res.Extras, res.Skipped))
	} else {
		res.Batch = ""
	}
	writeJSON(w, 200, res)
}

// Перейменування штатки: міняє поле sheet в усіх її посадах.
func renameSheet(w http.ResponseWriter, r *http.Request) {
	var in struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, "некоректний JSON")
		return
	}
	in.From, in.To = strings.TrimSpace(in.From), strings.TrimSpace(in.To)
	if in.To == "" {
		writeErr(w, 400, "порожня назва штатки")
		return
	}
	if in.From == in.To {
		writeJSON(w, 200, map[string]int64{"posts": 0})
		return
	}
	// Головна штатка не має назви, а посади, залиті до появи штаток, поля
	// sheet взагалі не мають — фільтр за "" їх не бачив і перейменування
	// мовчки не робило нічого.
	filter := bson.M{"sheet": in.From}
	if in.From == "" {
		filter = bson.M{"$or": []bson.M{
			{"sheet": ""}, {"sheet": nil}, {"sheet": bson.M{"$exists": false}},
		}}
	}
	res, err := posts.UpdateMany(r.Context(), filter,
		bson.M{"$set": bson.M{"sheet": in.To}})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	addLog(r.Context(), "", "—", "Штатка",
		fmt.Sprintf("перейменовано «%s» → «%s», посад: %d", dash(in.From), in.To, res.ModifiedCount))
	writeJSON(w, 200, map[string]int64{"posts": res.ModifiedCount})
}

func listImports(w http.ResponseWriter, r *http.Request) {
	cur, err := imports.Find(r.Context(), bson.M{},
		options.Find().SetSort(bson.D{{Key: "ts", Value: -1}}).SetLimit(10))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := []ImportBatch{}
	if err := cur.All(r.Context(), &out); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, out)
}

// Скасування імпорту: видаляє рівно те, що ним створено. Людей, яких потім
// перемістили чи змінили, це теж прибирає — вони зʼявились цим імпортом.
func undoImport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("batch")
	oid, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		writeErr(w, 400, "некоректна мітка імпорту")
		return
	}
	ctx := r.Context()
	dp, err := persons.DeleteMany(ctx, bson.M{"batch": id})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	dpo, err := posts.DeleteMany(ctx, bson.M{"batch": id})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if _, err := imports.DeleteOne(ctx, bson.M{"_id": oid}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	addLog(ctx, "", "—", "Скасовано імпорт",
		fmt.Sprintf("посад: %d, людей: %d", dpo.DeletedCount, dp.DeletedCount))
	writeJSON(w, 200, map[string]int64{"posts": dpo.DeletedCount, "persons": dp.DeletedCount})
}

// ---------- Скидання ----------

const resetWord = "СКИНУТИ"

// Вибіркове очищення бази. Незворотне, тому вимагає точного слова підтвердження.
func resetData(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Posts   bool   `json:"posts"`
		Persons bool   `json:"persons"`
		Journal bool   `json:"journal"`
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, "некоректний JSON")
		return
	}
	if strings.TrimSpace(in.Confirm) != resetWord {
		writeErr(w, 400, "для підтвердження введіть «"+resetWord+"»")
		return
	}
	if !in.Posts && !in.Persons && !in.Journal {
		writeErr(w, 400, "нічого не вибрано")
		return
	}
	ctx := r.Context()
	out := map[string]int64{}
	parts := []string{}

	if in.Persons {
		d, err := persons.DeleteMany(ctx, bson.M{})
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		out["persons"] = d.DeletedCount
		parts = append(parts, fmt.Sprintf("людей: %d", d.DeletedCount))
	}
	if in.Posts {
		d, err := posts.DeleteMany(ctx, bson.M{})
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		out["posts"] = d.DeletedCount
		parts = append(parts, fmt.Sprintf("посад: %d", d.DeletedCount))
		if !in.Persons {
			// Щоб люди не лишились на неіснуючих посадах — переводимо в позаштатні.
			u, err := persons.UpdateMany(ctx, bson.M{"postId": bson.M{"$ne": ""}}, bson.M{"$set": bson.M{
				"postId": "", "positionTag": "", "updatedAt": time.Now().UTC(),
			}})
			if err != nil {
				writeErr(w, 500, err.Error())
				return
			}
			out["detached"] = u.ModifiedCount
			parts = append(parts, fmt.Sprintf("у позаштат: %d", u.ModifiedCount))
		}
	}
	if in.Posts || in.Persons {
		_, _ = imports.DeleteMany(ctx, bson.M{}) // сліди вказували б на видалене
	}
	if in.Journal {
		d, err := journal.DeleteMany(ctx, bson.M{})
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		out["journal"] = d.DeletedCount
		parts = append(parts, fmt.Sprintf("журнал: %d", d.DeletedCount))
	}
	// Пишемо вже після очистки, щоб запис лишився.
	addLog(ctx, "", "—", "Скидання", strings.Join(parts, ", "))
	writeJSON(w, 200, out)
}

// ---------- Відсутність ----------

func addAbsence(w http.ResponseWriter, r *http.Request) {
	p, err := getPerson(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	var a Absence
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		writeErr(w, 400, "некоректний JSON")
		return
	}
	a.Kind = strings.TrimSpace(a.Kind)
	if a.Kind == "" {
		writeErr(w, 400, "вкажіть вид відсутності")
		return
	}
	if a.To != "" && a.From != "" && a.To < a.From {
		writeErr(w, 400, "дата «по» раніша за «з»")
		return
	}
	a.ID = primitive.NewObjectID().Hex()
	if _, err := persons.UpdateByID(r.Context(), p.ID, bson.M{
		"$push": bson.M{"absences": a},
		"$set":  bson.M{"updatedAt": time.Now().UTC()},
	}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	addLog(r.Context(), p.ID.Hex(), p.PIB, "Відсутність",
		strings.TrimSpace(fmt.Sprintf("%s з %s по %s %s", a.Kind, dash(a.From), dash(a.To), a.Place)))
	writeJSON(w, 201, a)
}

func deleteAbsence(w http.ResponseWriter, r *http.Request) {
	p, err := getPerson(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	absID := r.PathValue("absId")
	var removed *Absence
	for i := range p.Absences {
		if p.Absences[i].ID == absID {
			removed = &p.Absences[i]
			break
		}
	}
	if removed == nil {
		writeErr(w, 404, "запис про відсутність не знайдено")
		return
	}
	if _, err := persons.UpdateByID(r.Context(), p.ID, bson.M{
		"$pull": bson.M{"absences": bson.M{"id": absID}},
		"$set":  bson.M{"updatedAt": time.Now().UTC()},
	}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	addLog(r.Context(), p.ID.Hex(), p.PIB, "У строю", "знято: "+removed.Kind)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// ---------- Журнал ----------

func listLog(w http.ResponseWriter, r *http.Request) {
	filter := bson.M{}
	if pid := r.URL.Query().Get("personId"); pid != "" {
		filter["personId"] = pid
	}
	cur, err := journal.Find(r.Context(), filter,
		options.Find().SetSort(bson.D{{Key: "ts", Value: -1}}).SetLimit(500))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := []LogEntry{}
	if err := cur.All(r.Context(), &out); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, out)
}

// ---------- main ----------

func main() {
	if k := os.Getenv("API_KEY"); k != "" {
		apiKey = k
	}
	uri := os.Getenv("MONGODB_URI")
	if uri == "" {
		log.Fatal("MONGODB_URI не задано")
	}
	dbName := os.Getenv("DB_NAME")
	if dbName == "" {
		dbName = "personnel"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		log.Fatal(err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		log.Fatal("MongoDB недоступна: ", err)
	}
	db := client.Database(dbName)
	posts = db.Collection("posts")
	persons = db.Collection("persons")
	journal = db.Collection("journal")
	imports = db.Collection("imports")

	// Разові операції над базою: зробив і вийшов, сервер не піднімаємо.
	if i := indexOf(os.Args, "-phones"); i > 0 && i+1 < len(os.Args) {
		runPhones(context.Background(), os.Args[i+1], indexOf(os.Args, "-apply") > 0)
		return
	}
	if i := indexOf(os.Args, "-units"); i > 0 && i+1 < len(os.Args) {
		runUnits(context.Background(), os.Args[i+1], indexOf(os.Args, "-apply") > 0)
		return
	}
	if i := indexOf(os.Args, "-positions"); i > 0 && i+1 < len(os.Args) {
		runPositions(context.Background(), os.Args[i+1], indexOf(os.Args, "-apply") > 0)
		return
	}
	if i := indexOf(os.Args, "-dump"); i > 0 && i+1 < len(os.Args) {
		runDump(context.Background(), os.Args[i+1])
		return
	}

	mux := http.NewServeMux()
	api := http.NewServeMux()
	api.HandleFunc("GET /api/posts", listPosts)
	api.HandleFunc("DELETE /api/posts/{id}", deletePost)
	api.HandleFunc("POST /api/import", importData)
	api.HandleFunc("POST /api/reset", resetData)
	api.HandleFunc("GET /api/imports", listImports)
	api.HandleFunc("DELETE /api/imports/{batch}", undoImport)
	api.HandleFunc("POST /api/sheets/rename", renameSheet)
	api.HandleFunc("GET /api/persons", listPersons)
	api.HandleFunc("POST /api/persons", createPerson)
	api.HandleFunc("PUT /api/persons/{id}", updatePerson)
	api.HandleFunc("DELETE /api/persons/{id}", deletePerson)
	api.HandleFunc("POST /api/persons/{id}/move", movePerson)
	api.HandleFunc("POST /api/persons/{id}/dismiss", dismissPerson)
	api.HandleFunc("POST /api/persons/{id}/restore", restorePerson)
	api.HandleFunc("POST /api/persons/{id}/certs", addCert)
	api.HandleFunc("DELETE /api/persons/{id}/certs/{certId}", deleteCert)
	api.HandleFunc("POST /api/persons/{id}/absences", addAbsence)
	api.HandleFunc("DELETE /api/persons/{id}/absences/{absId}", deleteAbsence)
	api.HandleFunc("GET /api/log", listLog)
	mux.Handle("/api/", auth(api))

	var staticHandler http.Handler
	if os.Getenv("DEV") != "" {
		staticHandler = http.FileServer(http.Dir("static"))
		log.Println("DEV: статика з диска ./static")
	} else {
		staticRoot, _ := fs.Sub(staticFS, "static")
		staticHandler = http.FileServer(http.FS(staticRoot))
	}
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		staticHandler.ServeHTTP(w, r)
	}))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("Сервер на :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}
