package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	. "github.com/ahmetb/go-linq"
	_ "github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/labstack/echo"
	"github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/middleware"
	"html/template"
	"io"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type User struct {
	ID        int64  `json:"id,omitempty"`
	Nickname  string `json:"nickname,omitempty"`
	LoginName string `json:"login_name,omitempty"`
	PassHash  string `json:"pass_hash,omitempty"`
}

type Event struct {
	ID       int64  `json:"id,omitempty"`
	Title    string `json:"title,omitempty"`
	PublicFg bool   `json:"public,omitempty"`
	ClosedFg bool   `json:"closed,omitempty"`
	Price    int64  `json:"price,omitempty"`

	Total   int                `json:"total"`
	Remains int                `json:"remains"`
	Sheets  map[string]*Sheets `json:"sheets,omitempty"`
}

type Sheets struct {
	Total   int      `json:"total"`
	Remains int      `json:"remains"`
	Detail  []*Sheet `json:"detail,omitempty"`
	Price   int64    `json:"price"`
}

type Sheet struct {
	ID    int64  `json:"-"`
	Rank  string `json:"-"`
	Num   int64  `json:"num"`
	Price int64  `json:"-"`

	Mine           bool       `json:"mine,omitempty"`
	Reserved       bool       `json:"reserved,omitempty"`
	ReservedAt     *time.Time `json:"-"`
	ReservedAtUnix int64      `json:"reserved_at,omitempty"`
}

type Reservation struct {
	ID         int64      `json:"id"`
	EventID    int64      `json:"-"`
	SheetID    int64      `json:"-"`
	UserID     int64      `json:"-"`
	ReservedAt *time.Time `json:"-"`
	CanceledAt *time.Time `json:"-"`

	Event          *Event `json:"event,omitempty"`
	SheetRank      string `json:"sheet_rank,omitempty"`
	SheetNum       int64  `json:"sheet_num,omitempty"`
	Price          int64  `json:"price,omitempty"`
	ReservedAtUnix int64  `json:"reserved_at,omitempty"`
	CanceledAtUnix int64  `json:"canceled_at,omitempty"`
}

type Administrator struct {
	ID        int64  `json:"id,omitempty"`
	Nickname  string `json:"nickname,omitempty"`
	LoginName string `json:"login_name,omitempty"`
	PassHash  string `json:"pass_hash,omitempty"`
}

type SheetConfig struct {
	ID    int64
	Count int64
	Price int64
}

var SheetConfigs map[string]SheetConfig = map[string]SheetConfig{
	"S": SheetConfig{1, 50, 5000},
	"A": SheetConfig{51, 150, 3000},
	"B": SheetConfig{201, 300, 1000},
	"C": SheetConfig{501, 500, 0},
}

func getSheetRange(rank string) (int64, int64) {
	sc := SheetConfigs[rank]
	return sc.ID, sc.ID + sc.Count
}

var DefaultSheets []*Sheet

func getSheetFromId(id int64) *Sheet {
	sheet := DefaultSheets[id-1]
	return sheet
}

func sessUser(c echo.Context) *User {
	sess, _ := session.Get("session", c)
	if x, ok := sess.Values["user"]; ok {
		jsonStr := x.([]byte)
		user := &User{}
		json.Unmarshal(jsonStr, user)
		return user
	}
	return nil
}

func sessSetUser(c echo.Context, u *User) {
	sess, _ := session.Get("session", c)
	sess.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   3600,
		HttpOnly: true,
	}

	nu := &User{
		ID: u.ID,
		Nickname: u.Nickname,
	}
	sess.Values["user"], _ = json.Marshal(nu)
	sess.Save(c.Request(), c.Response())
}

func sessDeleteUser(c echo.Context) {
	sess, _ := session.Get("session", c)
	sess.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   3600,
		HttpOnly: true,
	}
	delete(sess.Values, "user")
	sess.Save(c.Request(), c.Response())
}

func sessAdministratorID(c echo.Context) int64 {
	sess, _ := session.Get("session", c)
	var administratorID int64
	if x, ok := sess.Values["administrator_id"]; ok {
		administratorID, _ = x.(int64)
	}
	return administratorID
}

func sessSetAdministratorID(c echo.Context, id int64) {
	sess, _ := session.Get("session", c)
	sess.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   3600,
		HttpOnly: true,
	}
	sess.Values["administrator_id"] = id
	sess.Save(c.Request(), c.Response())
}

func sessDeleteAdministratorID(c echo.Context) {
	sess, _ := session.Get("session", c)
	sess.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   3600,
		HttpOnly: true,
	}
	delete(sess.Values, "administrator_id")
	sess.Save(c.Request(), c.Response())
}

func loginRequired(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if _, err := getLoginUser(c); err != nil {
			return resError(c, "login_required", 401)
		}
		return next(c)
	}
}

func adminLoginRequired(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if _, err := getLoginAdministrator(c); err != nil {
			return resError(c, "admin_login_required", 401)
		}
		return next(c)
	}
}

func getLoginUser(c echo.Context) (*User, error) {
	user := sessUser(c)
	if user == nil {
		return nil, errors.New("not logged in")
	}
	return user, nil
}

func getLoginAdministrator(c echo.Context) (*Administrator, error) {
	administratorID := sessAdministratorID(c)
	if administratorID == 0 {
		return nil, errors.New("not logged in")
	}
	var administrator Administrator
	err := db.QueryRow("SELECT id, nickname FROM administrators WHERE id = ?", administratorID).Scan(&administrator.ID, &administrator.Nickname)
	return &administrator, err
}

var (
	reservationStore = make([]*Reservation, 0)
	reservationMutex = new(sync.Mutex)
)

func initReservation() error {
	rows, err := db.Query("SELECT * FROM reservations")
	if err != nil {
		return err
	}
	defer rows.Close()

	reservationStore = make([]*Reservation, 0)

	for rows.Next() {
		var reservation Reservation
		rows.Scan(
			&reservation.ID,
			&reservation.EventID,
			&reservation.SheetID,
			&reservation.UserID,
			&reservation.ReservedAt,
			&reservation.CanceledAt)
		reservationStore = append(reservationStore, &reservation)
	}

	return nil
}

var (
	eventStore = make([]*Event, 0)
	eventMutex = new(sync.Mutex)
)

func initEvents() error {
	rows, err := db.Query("SELECT * FROM events ORDER BY id ASC")
	if err != nil {
		return err
	}
	defer rows.Close()

	eventStore = make([]*Event, 0)
	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.ID, &event.Title, &event.PublicFg, &event.ClosedFg, &event.Price); err != nil {
			return err
		}
		eventStore = append(eventStore, &event)
	}

	return nil
}

func getEvents(all bool) ([]*Event, error) {
	var events []*Event
	for _, event := range eventStore {
		if !all && !event.PublicFg {
			continue
		}
		events = append(events, event)
	}
	for i, event := range events {
		event = fillEventOtherFields(event, -1)
		for k := range event.Sheets {
			event.Sheets[k].Detail = nil
		}
		events[i] = event
	}
	return events, nil
}

func fillEventOtherFields(e *Event, loginUserID int64) *Event {
	event := *e

	event.Sheets = map[string]*Sheets{
		"S": &Sheets{},
		"A": &Sheets{},
		"B": &Sheets{},
		"C": &Sheets{},
	}

	reservationsMap := make(map[int64]*Reservation)

	var reservations []*Reservation
	From(reservationStore).Where(func(c interface{}) bool {
		r := c.(*Reservation)
		if r.EventID != event.ID {
			return false
		}
		if r.CanceledAt != nil {
			return false
		}
		return true
	}).ToSlice(&reservations)

	for _, r := range reservations {
		reservationsMap[r.SheetID] = r
	}

	event.Total = 1000
	event.Remains = 0
	for rank, sc := range SheetConfigs {
		event.Sheets[rank].Total = int(sc.Count)
		event.Sheets[rank].Price = event.Price + sc.Price
		event.Sheets[rank].Remains = 0
		event.Sheets[rank].Detail = make([]*Sheet, 0)
	}


	for _, s := range DefaultSheets {
		var sheet = Sheet{
			ID:    s.ID,
			Rank:  s.Rank,
			Num:   s.Num,
			Price: s.Price,
		}
		reservation, ok := reservationsMap[s.ID]

		if !ok {
			event.Remains++
			event.Sheets[sheet.Rank].Remains++
		} else {
			sheet.Mine = reservation.UserID == loginUserID
			sheet.Reserved = true
			sheet.ReservedAtUnix = reservation.ReservedAt.Unix()
		}

		event.Sheets[sheet.Rank].Detail = append(event.Sheets[sheet.Rank].Detail, &sheet)
	}
	return &event
}

func getEvent(eventID, loginUserID int64) (*Event, error) {
	if eventID <= 0 || len(eventStore) <= int(eventID) - 1 {
		return nil, sql.ErrNoRows
	}
	e := eventStore[eventID - 1]
	return fillEventOtherFields(e, loginUserID), nil
}

func updateEvent(eventID int64, public bool, closed bool) {
	if eventID <= 0 || len(eventStore) <= int(eventID) - 1 {
		return
	}
	e := eventStore[eventID - 1]
	e.PublicFg = public
	e.ClosedFg = closed
}

func sanitizeEvent(e *Event) *Event {
	sanitized := *e
	sanitized.Price = 0
	sanitized.PublicFg = false
	sanitized.ClosedFg = false
	return &sanitized
}

func fillinUser(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if user, err := getLoginUser(c); err == nil {
			c.Set("user", user)
		}
		return next(c)
	}
}

func fillinAdministrator(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if administrator, err := getLoginAdministrator(c); err == nil {
			c.Set("administrator", administrator)
		}
		return next(c)
	}
}

func validateRank(rank string) bool {
	_, ok := SheetConfigs[rank]
	return ok
}

type Renderer struct {
	templates *template.Template
}

func (r *Renderer) Render(w io.Writer, name string, data interface{}, c echo.Context) error {
	return r.templates.ExecuteTemplate(w, name, data)
}

var db *sql.DB

func Getenv(key, fallback string) string {
	ret := os.Getenv(key)
	if ret != "" {
		return ret
	} else {
		return fallback
	}
}

func main() {
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&charset=utf8mb4",
		Getenv("DB_USER", "isucon"), Getenv("DB_PASS", "isucon"),
		Getenv("DB_HOST", "127.0.0.1"), Getenv("DB_PORT", "3306"),
		Getenv("DB_DATABASE", "torb"),
	)

	var err error
	db, err = sql.Open("mysql", dsn)
	if err != nil {
		log.Fatal(err)
	}

	initReservation()
	initEvents()

	// DefaultSheets
	DefaultSheets = make([]*Sheet, 0, 1000)
	for _, rank := range []string{"S", "A", "B", "C"} {
		c := SheetConfigs[rank]
		for num := int64(1); num <= c.Count; num++ {
			DefaultSheets = append(DefaultSheets, &Sheet{
				ID:    c.ID + num - 1,
				Rank:  rank,
				Num:   num,
				Price: c.Price,
			})
		}
	}

	e := echo.New()
	funcs := template.FuncMap{
		"encode_json": func(v interface{}) string {
			b, _ := json.Marshal(v)
			return string(b)
		},
	}
	e.Renderer = &Renderer{
		templates: template.Must(template.New("").Delims("[[", "]]").Funcs(funcs).ParseGlob("views/*.tmpl")),
	}
	e.Use(session.Middleware(sessions.NewCookieStore([]byte("secret"))))
	e.Use(middleware.LoggerWithConfig(middleware.LoggerConfig{Output: os.Stderr}))
	e.Static("/", "public")
	e.GET("/", func(c echo.Context) error {
		events, err := getEvents(false)
		if err != nil {
			return err
		}
		for i, v := range events {
			events[i] = sanitizeEvent(v)
		}
		return c.Render(200, "index.tmpl", echo.Map{
			"events": events,
			"user":   c.Get("user"),
			"origin": c.Scheme() + "://" + c.Request().Host,
		})
	}, fillinUser)
	e.GET("/debug/initReservation", func(c echo.Context) error {
		initReservation()
		return c.NoContent(204)
	})
	e.GET("/debug/initEvents", func(c echo.Context) error {
		initEvents()
		return c.NoContent(204)
	})

	e.GET("/initialize", func(c echo.Context) error {
		cmd := exec.Command("../../db/init.sh")
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		err := cmd.Run()
		if err != nil {
			return nil
		}

		initReservation()
		initEvents()

		return c.NoContent(204)
	})
	e.POST("/api/users", func(c echo.Context) error {
		var params struct {
			Nickname  string `json:"nickname"`
			LoginName string `json:"login_name"`
			Password  string `json:"password"`
		}
		c.Bind(&params)

		tx, err := db.Begin()
		if err != nil {
			return err
		}

		var user User
		if err := tx.QueryRow("SELECT * FROM users WHERE login_name = ?", params.LoginName).Scan(&user.ID, &user.LoginName, &user.Nickname, &user.PassHash); err != sql.ErrNoRows {
			tx.Rollback()
			if err == nil {
				return resError(c, "duplicated", 409)
			}
			return err
		}

		res, err := tx.Exec("INSERT INTO users (login_name, pass_hash, nickname) VALUES (?, SHA2(?, 256), ?)", params.LoginName, params.Password, params.Nickname)
		if err != nil {
			tx.Rollback()
			return resError(c, "", 0)
		}
		userID, err := res.LastInsertId()
		if err != nil {
			tx.Rollback()
			return resError(c, "", 0)
		}
		if err := tx.Commit(); err != nil {
			return err
		}

		return c.JSON(201, echo.Map{
			"id":       userID,
			"nickname": params.Nickname,
		})
	})
	e.GET("/api/users/:id", func(c echo.Context) error {
		var user User
		if err := db.QueryRow("SELECT id, nickname FROM users WHERE id = ?", c.Param("id")).Scan(&user.ID, &user.Nickname); err != nil {
			return err
		}

		loginUser, err := getLoginUser(c)
		if err != nil {
			return err
		}
		if user.ID != loginUser.ID {
			return resError(c, "forbidden", 403)
		}

		var relatedReservations []*Reservation
		From(reservationStore).Where(func(c interface{}) bool {
			r := c.(*Reservation)
			if r.UserID != user.ID {
				return false
			}
			return true
		}).OrderByDescending(func(c interface{}) interface{} {
			r := c.(*Reservation)
			if r.CanceledAt != nil {
				return r.CanceledAt.UnixNano()
			} else {
				return r.ReservedAt.UnixNano()
			}
		}).ToSlice(&relatedReservations)

		var recentReservations []*Reservation
		From(relatedReservations).Take(5).ToSlice(&recentReservations)

		for _, reservation := range (recentReservations) {
			sheet := getSheetFromId(reservation.SheetID)

			event, err := getEvent(reservation.EventID, -1)
			if err != nil {
				return err
			}
			price := event.Sheets[sheet.Rank].Price
			event.Sheets = nil
			event.Total = 0
			event.Remains = 0

			reservation.Event = event
			reservation.SheetRank = sheet.Rank
			reservation.SheetNum = sheet.Num
			reservation.Price = price
			reservation.ReservedAtUnix = reservation.ReservedAt.Unix()
			if reservation.CanceledAt != nil {
				reservation.CanceledAtUnix = reservation.CanceledAt.Unix()
			}
		}

		totalPrice := 0
		for _,reservation := range(relatedReservations) {
			if reservation.CanceledAt != nil {
				continue
			}
			sheet := getSheetFromId(reservation.SheetID)
			event, err := getEvent(reservation.EventID, -1)
			if err != nil {
				log.Println(err)
				return err
			}
			curPrice := event.Price + sheet.Price

			totalPrice += int(curPrice)
		}

		var eventIds []int64
		From(relatedReservations).GroupBy(func(c interface{}) interface{}{
			r := c.(*Reservation)
			return r.EventID
		}, func(c interface{}) interface{} {
			return c
		}).OrderByDescending(func(c interface{}) interface{} {
			values := c.(Group).Group
			maxTime := From(values).Select(func(c2 interface{}) interface{} {
				r := c2.(*Reservation)
				if r.CanceledAt != nil {
					return r.CanceledAt.UnixNano()
				} else {
					return r.ReservedAt.UnixNano()
				}
			}).Max().(int64)

			return maxTime
		}).Take(5).Select(func(c interface{}) interface{} {
			eventId := c.(Group).Key
			return eventId
		}).ToSlice(&eventIds)


		var recentEvents []*Event
		for _, eventID := range(eventIds) {
			event, err := getEvent(eventID, -1)
			if err != nil {
				return err
			}
			for k := range event.Sheets {
				event.Sheets[k].Detail = nil
			}
			recentEvents = append(recentEvents, event)
		}
		if recentEvents == nil {
			recentEvents = make([]*Event, 0)
		}

		return c.JSON(200, echo.Map{
			"id":                  user.ID,
			"nickname":            user.Nickname,
			"recent_reservations": recentReservations,
			"total_price":         totalPrice,
			"recent_events":       recentEvents,
		})
	}, loginRequired)
	e.POST("/api/actions/login", func(c echo.Context) error {
		var params struct {
			LoginName string `json:"login_name"`
			Password  string `json:"password"`
		}
		c.Bind(&params)

		user := new(User)
		if err := db.QueryRow("SELECT * FROM users WHERE login_name = ?", params.LoginName).Scan(&user.ID, &user.Nickname, &user.LoginName, &user.PassHash); err != nil {
			if err == sql.ErrNoRows {
				return resError(c, "authentication_failed", 401)
			}
			return err
		}

		var passHash string
		if err := db.QueryRow("SELECT SHA2(?, 256)", params.Password).Scan(&passHash); err != nil {
			return err
		}
		if user.PassHash != passHash {
			return resError(c, "authentication_failed", 401)
		}

		sessSetUser(c, user)
		user, err = getLoginUser(c)
		if err != nil {
			return err
		}
		return c.JSON(200, user)
	})
	e.POST("/api/actions/logout", func(c echo.Context) error {
		sessDeleteUser(c)
		return c.NoContent(204)
	}, loginRequired)
	e.GET("/api/events", func(c echo.Context) error {
		events, err := getEvents(true)
		if err != nil {
			return err
		}
		for i, v := range events {
			events[i] = sanitizeEvent(v)
		}
		return c.JSON(200, events)
	})
	e.GET("/api/events/:id", func(c echo.Context) error {
		eventID, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			return resError(c, "not_found", 404)
		}

		loginUserID := int64(-1)
		if user, err := getLoginUser(c); err == nil {
			loginUserID = user.ID
		}

		event, err := getEvent(eventID, loginUserID)
		if err != nil {
			if err == sql.ErrNoRows {
				return resError(c, "not_found", 404)
			}
			return err
		} else if !event.PublicFg {
			return resError(c, "not_found", 404)
		}
		return c.JSON(200, sanitizeEvent(event))
	})
	e.POST("/api/events/:id/actions/reserve", func(c echo.Context) error {
		eventID, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			return resError(c, "not_found", 404)
		}
		var params struct {
			Rank string `json:"sheet_rank"`
		}
		c.Bind(&params)

		user, err := getLoginUser(c)
		if err != nil {
			return err
		}

		event, err := getEvent(eventID, user.ID)
		if err != nil {
			if err == sql.ErrNoRows {
				return resError(c, "invalid_event", 404)
			}
			return err
		} else if !event.PublicFg {
			return resError(c, "invalid_event", 404)
		}

		if !validateRank(params.Rank) {
			return resError(c, "invalid_rank", 400)
		}

		sheetIdL, sheetIdR := getSheetRange(params.Rank)
		usedSheets := make([]bool, sheetIdR-sheetIdL)

		var sheet *Sheet
		var reservationID int64

		reservationMutex.Lock()
		{
			defer reservationMutex.Unlock()
			var reservations []*Reservation
			From(reservationStore).Where(func(c interface{}) bool {
				r := c.(*Reservation)
				if r.EventID != event.ID {
					return false
				}
				if r.CanceledAt != nil {
					return false
				}
				return true
			}).ToSlice(&reservations)

			for _, reservation := range reservations {
				sheetId := reservation.SheetID
				if sheetIdL <= sheetId && sheetId < sheetIdR {
					usedSheets[sheetId-sheetIdL] = true
				}
			}

			idxes := make([]int64, sheetIdR-sheetIdL)
			for i := 0; i < len(idxes); i++ {
				idxes[i] = int64(i) + sheetIdL
			}
			for i := len(idxes) - 1; i >= 0; i-- {
				j := rand.Intn(i + 1)
				idxes[i], idxes[j] = idxes[j], idxes[i]
			}

			useSheetId := int64(-1)
			for i := 0; i < int(sheetIdR - sheetIdL); i++ {
				id := idxes[i]
				if !usedSheets[id-sheetIdL] {
					useSheetId = id
					break
				}
			}

			if useSheetId == -1 {
				return resError(c, "sold_out", 409)
			}
			sheet = getSheetFromId(useSheetId)

			reservation := &Reservation{}

			reservation.ID = int64(len(reservationStore) + 1)
			reservationID = reservation.ID
			reservation.EventID = event.ID
			reservation.SheetID = sheet.ID
			reservation.UserID = user.ID
			reservationTime := time.Now().UTC()
			reservation.ReservedAt = &reservationTime

			reservationStore = append(reservationStore, reservation)

			go func() {
				res, err := db.Exec("INSERT INTO reservations (id, event_id, sheet_id, user_id, reserved_at) VALUES (?, ?, ?, ?, ?)",
					reservation.ID, reservation.EventID, reservation.SheetID, reservation.UserID, reservation.ReservedAt.Format("2006-01-02 15:04:05.000000"))

				if err != nil {
					log.Println("error happened on Exec")
					return
				}

				reservationID, err := res.LastInsertId()
				if reservation.ID != reservationID {
					log.Println("reservation ID mismatch")
					return
				}
			}()

		}

		return c.JSON(202, echo.Map{
			"id":         reservationID,
			"sheet_rank": params.Rank,
			"sheet_num":  sheet.Num,
		})
	}, loginRequired)
	e.DELETE("/api/events/:id/sheets/:rank/:num/reservation", func(c echo.Context) error {
		eventID, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			return resError(c, "not_found", 404)
		}
		rank := c.Param("rank")
		num := c.Param("num")

		user, err := getLoginUser(c)
		if err != nil {
			return err
		}

		event, err := getEvent(eventID, user.ID)
		if err != nil {
			if err == sql.ErrNoRows {
				return resError(c, "invalid_event", 404)
			}
			return err
		} else if !event.PublicFg {
			return resError(c, "invalid_event", 404)
		}

		if !validateRank(rank) {
			return resError(c, "invalid_rank", 404)
		}

		sc, ok := SheetConfigs[rank]
		if !ok {
			return resError(c, "invalid_sheet", 404)
		}
		intNum, _ := strconv.ParseInt(num, 10, 64)
		if sc.Count < intNum {
			return resError(c, "invalid_sheet", 404)
		}
		sheetId := sc.ID + intNum - 1

		var reservations []*Reservation
		From(reservationStore).Where(func(c interface{}) bool {
			r := c.(*Reservation)
			if r.EventID != event.ID {
				return false
			}
			if r.SheetID != sheetId {
				return false
			}
			if r.CanceledAt != nil {
				return false
			}
			return true
		}).ToSlice(&reservations)

		if len(reservations) == 0 {
			return resError(c, "not_reserved", 400)
		}
		reservation := reservations[0]

		if reservation.UserID != user.ID {
			return resError(c, "not_permitted", 403)
		}

		canceledAt := time.Now().UTC()
		reservation.CanceledAt = &canceledAt

		go func() {
			if _, err := db.Exec("UPDATE reservations SET canceled_at = ? WHERE id = ?", canceledAt.Format("2006-01-02 15:04:05.000000"), reservation.ID); err != nil {
				log.Println("error happened on UPDATE reservations", err)
			}
		}()

		return c.NoContent(204)
	}, loginRequired)
	e.GET("/admin/", func(c echo.Context) error {
		var events []*Event
		administrator := c.Get("administrator")
		if administrator != nil {
			var err error
			if events, err = getEvents(true); err != nil {
				return err
			}
		}
		return c.Render(200, "admin.tmpl", echo.Map{
			"events":        events,
			"administrator": administrator,
			"origin":        c.Scheme() + "://" + c.Request().Host,
		})
	}, fillinAdministrator)
	e.POST("/admin/api/actions/login", func(c echo.Context) error {
		var params struct {
			LoginName string `json:"login_name"`
			Password  string `json:"password"`
		}
		c.Bind(&params)

		administrator := new(Administrator)
		if err := db.QueryRow("SELECT * FROM administrators WHERE login_name = ?", params.LoginName).Scan(&administrator.ID, &administrator.LoginName, &administrator.Nickname, &administrator.PassHash); err != nil {
			if err == sql.ErrNoRows {
				return resError(c, "authentication_failed", 401)
			}
			return err
		}

		var passHash string
		if err := db.QueryRow("SELECT SHA2(?, 256)", params.Password).Scan(&passHash); err != nil {
			return err
		}
		if administrator.PassHash != passHash {
			return resError(c, "authentication_failed", 401)
		}

		sessSetAdministratorID(c, administrator.ID)
		administrator, err = getLoginAdministrator(c)
		if err != nil {
			return err
		}
		return c.JSON(200, administrator)
	})
	e.POST("/admin/api/actions/logout", func(c echo.Context) error {
		sessDeleteAdministratorID(c)
		return c.NoContent(204)
	}, adminLoginRequired)
	e.GET("/admin/api/events", func(c echo.Context) error {
		events, err := getEvents(true)
		if err != nil {
			return err
		}
		return c.JSON(200, events)
	}, adminLoginRequired)
	e.POST("/admin/api/events", func(c echo.Context) error {
		var params struct {
			Title  string `json:"title"`
			Public bool   `json:"public"`
			Price  int    `json:"price"`
		}
		c.Bind(&params)

		eventMutex.Lock()
		event := &Event{}
		{
			defer eventMutex.Unlock()

			event.ID = int64(len(eventStore) + 1)
			event.Title = params.Title
			event.PublicFg = params.Public
			event.ClosedFg = false
			event.Price = int64(params.Price)

			eventStore = append(eventStore, event)
		}

		go func() {
			_, err := db.Exec("INSERT INTO events (id, title, public_fg, closed_fg, price) VALUES (?, ?, ?, ?, ?)",
				event.ID, event.Title, event.PublicFg, event.ClosedFg, event.Price)
			if err != nil {
				log.Println(err)
			}
		}()

		event = fillEventOtherFields(event, -1)
		return c.JSON(200, event)
	}, adminLoginRequired)
	e.GET("/admin/api/events/:id", func(c echo.Context) error {
		eventID, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			return resError(c, "not_found", 404)
		}
		event, err := getEvent(eventID, -1)
		if err != nil {
			if err == sql.ErrNoRows {
				return resError(c, "not_found", 404)
			}
			return err
		}
		return c.JSON(200, event)
	}, adminLoginRequired)
	e.POST("/admin/api/events/:id/actions/edit", func(c echo.Context) error {
		eventID, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			return resError(c, "not_found", 404)
		}

		var params struct {
			Public bool `json:"public"`
			Closed bool `json:"closed"`
		}
		c.Bind(&params)
		if params.Closed {
			params.Public = false
		}

		event, err := getEvent(eventID, -1)
		if err != nil {
			if err == sql.ErrNoRows {
				return resError(c, "not_found", 404)
			}
			return err
		}

		if event.ClosedFg {
			return resError(c, "cannot_edit_closed_event", 400)
		} else if event.PublicFg && params.Closed {
			return resError(c, "cannot_close_public_event", 400)
		}

		event.PublicFg = params.Public
		event.ClosedFg = params.Closed
		updateEvent(eventID, params.Public, params.Closed)

		go func() {
			event, _ := getEvent(eventID, -1)
			if _, err := db.Exec("UPDATE events SET public_fg = ?, closed_fg = ? WHERE id = ?", event.PublicFg, event.ClosedFg, event.ID); err != nil {
				log.Println(err)
			}
		}()

		c.JSON(200, event)
		return nil
	}, adminLoginRequired)
	e.GET("/admin/api/reports/events/:id/sales", func(c echo.Context) error {
		eventID, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			return resError(c, "not_found", 404)
		}

		event, err := getEvent(eventID, -1)
		if err != nil {
			return err
		}

		var reservations []*Reservation
		From(reservationStore).Where(func(c interface{}) bool {
			r := c.(*Reservation)
			if r.EventID != event.ID {
				return false
			}
			return true
		}).OrderBy(func(c interface{}) interface{} {
			r := c.(*Reservation)
			return r.ReservedAt.UnixNano()
		}).ToSlice(&reservations)

		var reports []Report
		for _, reservation := range reservations {
			sheet := getSheetFromId(reservation.SheetID)
			report := Report{
				ReservationID: reservation.ID,
				EventID:       event.ID,
				Rank:          sheet.Rank,
				Num:           sheet.Num,
				UserID:        reservation.UserID,
				SoldAt:        reservation.ReservedAt.Format("2006-01-02T15:04:05.000000Z"),
				Price:         event.Price + sheet.Price,
			}
			if reservation.CanceledAt != nil {
				report.CanceledAt = reservation.CanceledAt.Format("2006-01-02T15:04:05.000000Z")
			}
			reports = append(reports, report)
		}
		return renderReportCSV(c, reports)
	}, adminLoginRequired)
	e.GET("/admin/api/reports/sales", func(c echo.Context) error {
		var reservations []*Reservation
		From(reservationStore).OrderBy(func(c interface{}) interface{} {
			r := c.(*Reservation)
			return r.ReservedAt.UnixNano()
		}).ToSlice(&reservations)

		events, _ := getEvents(true)
		eventMap := make(map[int64]*Event)
		for _, event := range events {
			eventMap[event.ID] = event
		}

		var reports []Report
		for _, reservation := range reservations {
			event := eventMap[reservation.EventID]
			sheet := getSheetFromId(reservation.SheetID)

			report := Report{
				ReservationID: reservation.ID,
				EventID:       event.ID,
				Rank:          sheet.Rank,
				Num:           sheet.Num,
				UserID:        reservation.UserID,
				SoldAt:        reservation.ReservedAt.Format("2006-01-02T15:04:05.000000Z"),
				Price:         event.Price + sheet.Price,
			}
			if reservation.CanceledAt != nil {
				report.CanceledAt = reservation.CanceledAt.Format("2006-01-02T15:04:05.000000Z")
			}
			reports = append(reports, report)
		}
		return renderReportCSV(c, reports)
	}, adminLoginRequired)

	e.Start(":8080")
}

type Report struct {
	ReservationID int64
	EventID       int64
	Rank          string
	Num           int64
	UserID        int64
	SoldAt        string
	CanceledAt    string
	Price         int64
}

func renderReportCSV(c echo.Context, reports []Report) error {
	sort.Slice(reports, func(i, j int) bool { return strings.Compare(reports[i].SoldAt, reports[j].SoldAt) < 0 })

	body := bytes.NewBufferString("reservation_id,event_id,rank,num,price,user_id,sold_at,canceled_at\n")
	for _, v := range reports {
		body.WriteString(fmt.Sprintf("%d,%d,%s,%d,%d,%d,%s,%s\n",
			v.ReservationID, v.EventID, v.Rank, v.Num, v.Price, v.UserID, v.SoldAt, v.CanceledAt))
	}

	c.Response().Header().Set("Content-Type", `text/csv; charset=UTF-8`)
	c.Response().Header().Set("Content-Disposition", `attachment; filename="report.csv"`)
	_, err := io.Copy(c.Response(), body)
	return err
}

func resError(c echo.Context, e string, status int) error {
	if e == "" {
		e = "unknown"
	}
	if status < 100 {
		status = 500
	}
	return c.JSON(status, map[string]string{"error": e})
}
