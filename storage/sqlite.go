package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sqleo/parse-video/parser"
)

type Input struct {
	ShareURL string `json:"share_url,omitempty"`
	VideoID  string `json:"video_id,omitempty"`
}

type Record struct {
	ID        int64                  `json:"id"`
	Timestamp string                 `json:"ts"`
	Endpoint  string                 `json:"endpoint"`
	Source    string                 `json:"source,omitempty"`
	Input     Input                  `json:"input"`
	ClientIP  string                 `json:"client_ip,omitempty"`
	UserAgent string                 `json:"user_agent,omitempty"`
	Result    *parser.VideoParseInfo `json:"result,omitempty"`
	Error     string                 `json:"error,omitempty"`
}

type QueryOptions struct {
	Start    *time.Time
	End      *time.Time
	Source   string
	Endpoint string
	Contains string // 模糊匹配 share_url 或 video_id
	ClientIP string
	Limit    int
	Offset   int
}

var db *sql.DB

func Init(dbPath string) error {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return err
	}
	d, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	db = d
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL;`); err != nil {
		return err
	}
	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS records (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	ts TEXT NOT NULL,
	endpoint TEXT,
	source TEXT,
	share_url TEXT,
	video_id TEXT,
	client_ip TEXT,
	user_agent TEXT,
	title TEXT,
	video_url TEXT,
	music_url TEXT,
	cover_url TEXT,
	images_json TEXT,
	author_uid TEXT,
	author_name TEXT,
	author_avatar TEXT,
	error TEXT
);
CREATE INDEX IF NOT EXISTS idx_records_ts ON records(ts);
CREATE INDEX IF NOT EXISTS idx_records_source ON records(source);
CREATE INDEX IF NOT EXISTS idx_records_endpoint ON records(endpoint);
CREATE INDEX IF NOT EXISTS idx_records_client_ip ON records(client_ip);
`)
	return err
}

func Append(ctx context.Context, rec Record) error {
	if db == nil {
		return sql.ErrConnDone
	}
	ts := time.Now().Format(time.RFC3339)
	var title, videoURL, musicURL, coverURL, imagesJSON, auid, aname, aavatar string
	if rec.Result != nil {
		title = rec.Result.Title
		videoURL = rec.Result.VideoUrl
		musicURL = rec.Result.MusicUrl
		coverURL = rec.Result.CoverUrl
		if len(rec.Result.Images) > 0 {
			b, _ := json.Marshal(rec.Result.Images)
			imagesJSON = string(b)
		}
		auid = rec.Result.Author.Uid
		aname = rec.Result.Author.Name
		aavatar = rec.Result.Author.Avatar
	}
	_, err := db.ExecContext(ctx, `
INSERT INTO records(
	ts, endpoint, source, share_url, video_id,
	client_ip, user_agent, title, video_url, music_url, cover_url,
	images_json, author_uid, author_name, author_avatar, error
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		ts, rec.Endpoint, rec.Source, rec.Input.ShareURL, rec.Input.VideoID,
		rec.ClientIP, rec.UserAgent, title, videoURL, musicURL, coverURL,
		imagesJSON, auid, aname, aavatar, strings.TrimSpace(rec.Error),
	)
	return err
}

func Query(ctx context.Context, q QueryOptions) ([]Record, error) {
	if db == nil {
		return nil, sql.ErrConnDone
	}
	where := []string{"1=1"}
	args := []any{}

	if q.Start != nil {
		where = append(where, "ts >= ?")
		args = append(args, q.Start.Format(time.RFC3339))
	}
	if q.End != nil {
		where = append(where, "ts <= ?")
		args = append(args, q.End.Format(time.RFC3339))
	}
	if q.Source != "" {
		where = append(where, "source = ?")
		args = append(args, q.Source)
	}
	if q.Endpoint != "" {
		where = append(where, "endpoint = ?")
		args = append(args, q.Endpoint)
	}
	if q.ClientIP != "" {
		where = append(where, "client_ip = ?")
		args = append(args, q.ClientIP)
	}
	if q.Contains != "" {
		where = append(where, "(share_url LIKE ? OR video_id LIKE ?)")
		p := "%" + q.Contains + "%"
		args = append(args, p, p)
	}
	limit := q.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}

	rows, err := db.QueryContext(ctx, `
SELECT id, ts, endpoint, source, share_url, video_id, client_ip, user_agent,
       title, video_url, music_url, cover_url, images_json,
       author_uid, author_name, author_avatar, error
FROM records
WHERE `+strings.Join(where, " AND ")+`
ORDER BY id DESC
LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Record
	for rows.Next() {
		var (
			r                 Record
			shareURL, videoID string
			title, videoURL2, musicURL, coverURL, imagesJSON, auid, aname, aavatar, e string
		)
		if err := rows.Scan(
			&r.ID, &r.Timestamp, &r.Endpoint, &r.Source, &shareURL, &videoID, &r.ClientIP, &r.UserAgent,
			&title, &videoURL2, &musicURL, &coverURL, &imagesJSON,
			&auid, &aname, &aavatar, &e,
		); err != nil {
			return nil, err
		}
		r.Input = Input{ShareURL: shareURL, VideoID: videoID}
		if title != "" || videoURL2 != "" || coverURL != "" || imagesJSON != "" || auid != "" || aname != "" || aavatar != "" {
			r.Result = &parser.VideoParseInfo{
				Title:    title,
				VideoUrl: videoURL2,
				MusicUrl: musicURL,
				CoverUrl: coverURL,
			}
			_ = json.Unmarshal([]byte(imagesJSON), &r.Result.Images)
			r.Result.Author.Uid = auid
			r.Result.Author.Name = aname
			r.Result.Author.Avatar = aavatar
		}
		r.Error = e
		out = append(out, r)
	}
	return out, rows.Err()
}


