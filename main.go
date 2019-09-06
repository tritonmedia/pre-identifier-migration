package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/adlio/trello"
	"github.com/gofrs/uuid"
	"github.com/gogo/protobuf/proto"
	"github.com/jackc/pgx"
	"github.com/minio/minio-go/v6"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/tritonmedia/identifier/pkg/rabbitmq"
	api "github.com/tritonmedia/tritonmedia.go/pkg/proto"
)

const (
	// BucketName is the bucket to read files from
	BucketName = "triton-media"

	// TrelloBoard is the board to read from
	TrelloBoard = "5a65133a4c47f638cd4ff1e8"

	// TrelloList is the list to read cards frok
	TrelloList = "5a651367c5be24939d689c19"
)

var trelloClient *trello.Client
var pgClient *pgx.ConnPool
var urlParseRegex = regexp.MustCompile(`\[(\w+)\]\((.+)\)`)
var fileParseRegex = regexp.MustCompile(`S(\d+)E(\d+)`)
var amqpClient *rabbitmq.Client

func init() {
	trelloClient = trello.NewClient(os.Getenv("TRELLO_APPKEY"), os.Getenv("TRELLO_TOKEN"))

	pgEndpoint := os.Getenv("POSTGRES_ENDPOINT")
	if pgEndpoint == "" {
		pgEndpoint = "127.0.0.1"
		log.Warnf("POSTGRES_ENDPOINT not defined, defaulting to local config: %s", pgEndpoint)
	}

	var err error
	pgClient, err = pgx.NewConnPool(pgx.ConnPoolConfig{
		ConnConfig: pgx.ConnConfig{
			Host:     pgEndpoint,
			User:     "postgres",
			Database: "media",
		},
	})
	if err != nil {
		log.Errorf("failed to connect to postgres: %v", err)
	}
	log.Infof("connected to postgres")

	amqpClient, err = rabbitmq.NewClient("amqp://user:bitnami@127.0.0.1:5672")
	if err != nil {
		log.Fatalf("failed to connect to rabbitmq: %v", err)
	}
}

// insertCard inserts the card into our media storage, if it doesn't already exist,
// otherwise it updates the media object in the database to match the data from trello
func insertCard(c *trello.Card, metadataID string,
	metadataProvider api.Media_MetadataType, mediaType api.Media_MediaType, source api.Media_SourceType, sourceURI string) (string, error) {
	r, err := pgClient.Query(`
		SELECT id FROM media WHERE creator_id=$1
	`, c.ID)
	if err != nil {
		log.Warnf("failed to search for existing row: %v", err)
	}
	defer r.Close()

	r.Next()

	vals, err := r.Values()
	if err == nil {
		if len(vals) == 1 {
			mediaID := vals[0].(string)

			log.Infof("updating existing database entry for media '%s' (id: %s)", c.Name, mediaID)
			_, err = pgClient.Exec(`
				UPDATE media SET metadata_id=$1, metadata=$2 WHERE id=$3
			`, metadataID, metadataProvider, mediaID)
			return mediaID, err
		}
	}

	id, err := uuid.NewV4()
	if err != nil {
		return "", errors.Wrap(err, "failed to generate id for episode")
	}

	// TODO(jaredallard): get real media type
	// TODO(jaredallard): get source type
	log.Infof("creating database entry for media '%s'", c.Name)
	_, err = pgClient.Exec(`
		INSERT INTO media
			(id, media_name, creator, creator_id, type, source, source_uri, metadata_id, metadata, status)
		VALUES
			($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, id.String(), c.Name, 1, c.ID, mediaType, source, sourceURI, metadataID, metadataProvider, 5)
	return id.String(), err
}

// findEpisodes scans S3 to find episodes and extract information to mimic what Twilight would send
// to identifier
func findEpisodes(mediaType api.Media_MediaType, mediaID string, mediaName string) error {
	var ssl bool
	u, err := url.Parse(os.Getenv("S3_ENDPOINT"))
	if err != nil {
		return errors.Wrap(err, "failed to parse minio endpoint")
	}

	if u.Scheme == "https" {
		ssl = true
	}

	m, err := minio.New(
		u.Host,
		os.Getenv("S3_ACCESS_KEY"),
		os.Getenv("S3_SECRET_KEY"),
		ssl,
	)
	if err != nil {
		return err
	}

	// Create a done channel to control 'ListObjects' go routine.
	doneCh := make(chan struct{})

	// Indicate to our routine to exit cleanly upon return.
	defer close(doneCh)

	mediaTypeStr := mediaType.String()
	if mediaType == api.Media_MOVIE {
		// HACK: This fixes the disparity in libraries for TRITON only
		mediaTypeStr = "movies"
	}

	files := make([]api.IdentifyNewFile, 0)

	key := fmt.Sprintf("%s/%s", strings.ToLower(mediaTypeStr), mediaName)
	log.Infof("searching for media files for '%s' in '%s'", mediaName, key)
	objectCh := m.ListObjects(BucketName, key, true, doneCh)
	for object := range objectCh {
		if object.Err != nil {
			log.Errorf("failed to read objects: %v", err)
			break
		}

		// TODO(jaredallard): assumes mkv
		if !strings.HasSuffix(object.Key, ".mkv") {
			continue
		}

		var season int
		var episode int
		if mediaType != api.Media_MOVIE {
			matches := fileParseRegex.FindStringSubmatch(object.Key)
			if len(matches) < 3 {
				continue
			}

			season, err = strconv.Atoi(matches[1])
			if err != nil {
				continue
			}
			episode, err = strconv.Atoi(matches[2])
			if err != nil {
				continue
			}
		} else {
			// Special handling
			season = 0
			episode = 0
		}

		log.Infof("found season %d episode %d in '%s'", season, episode, filepath.Base(object.Key))
		files = append(files, api.IdentifyNewFile{
			CreatedAt: time.Now().Format(time.RFC3339),

			// TODO(jaredallard): both twilight and us need to do a better job at this
			Media: &api.Media{
				Id:   mediaID,
				Type: mediaType,
			},

			Key:     object.Key,
			Episode: int64(episode),
			Season:  int64(season),
		})

		if mediaType == api.Media_MOVIE {
			break
		}
	}

	if len(files) == 0 {
		return fmt.Errorf("failed to find any media files")
	}

	for _, f := range files {
		b, err := proto.Marshal(&f)
		if err != nil {
			log.Errorf("failed to create protobuf encoded message for identifier.newfile: %v", err)
			continue
		}

		if err := amqpClient.Publish("v1.identify.newfile", b); err != nil {
			log.Errorf("failed to publish message to rabbitmq: %v", err)
			continue
		}
	}

	return nil
}

func main() {
	l, err := trelloClient.GetList(TrelloList, trello.Defaults())
	if err != nil {
		log.Fatalf("failed to read trello list: %v", err)
	}

	log.Infof("listing cards in list '%s' (id: %s)", l.Name, l.ID)
	cards, err := l.GetCards(trello.Arguments{"attachments": "true"})
	if err != nil {
		log.Fatalf("failed to read cards on list '%s': %v", l.ID, err)
	}

	for _, c := range cards {
		if strings.Contains(c.Name, "Season") {
			log.Warnf("skipping redudant season card")
			continue
		}

		var metadataProviderName api.Media_MetadataType
		metadataID := ""
		for _, a := range c.Attachments {
			log.Debugf("scanning attachment '%s'", a.Name)
			switch a.Name {
			case "TVDB":
				metadataProviderName = api.Media_TVDB
				metadataID = strings.Split(a.URL, "/")[4]
				break
			case "TMDB":
				metadataProviderName = api.Media_TMDB
				metadataID = strings.Split(a.URL, "/")[4]
				break
			case "IMDB":
				metadataProviderName = api.Media_IMDB
				metadataID = strings.Split(a.URL, "/")[4]
				break
			}
		}

		mediaType := api.Media_TV
		for _, l := range c.Labels {
			switch l.Name {
			case "Movie":
				log.Infof("setting media type to Movie")
				mediaType = api.Media_MOVIE
			}
		}

		matches := urlParseRegex.FindStringSubmatch(c.Desc)
		if len(matches) < 2 {
			log.Errorf("skipping invalid card '%s' (desc)", c.Name)
			continue
		}

		sourceURI := matches[2]
		u, err := url.Parse(sourceURI)
		if err != nil {
			log.Errorf("skipping invalid card '%s' (desc::url-parse): %v", c.Name, err)
			continue
		}

		if u.Scheme == "magnet" {
			u.Scheme = "torrent"
		}

		if u.Scheme == "https" {
			u.Scheme = "http"
		}

		s, ok := api.Media_SourceType_value[strings.ToUpper(u.Scheme)]
		if !ok {
			log.Errorf("skipping invalid card '%s' (desc::url-parse): invalid scheme '%s'", c.Name, u.Scheme)
			continue
		}

		sourceType := api.Media_SourceType(s)

		log.Infof("processing card: name='%s',type=%d,provider='%s',provider_id=%s,source=%s,source_uri=%s", c.Name, mediaType, metadataProviderName.String(), metadataID, sourceType.String(), string(sourceURI[0:20]))
		if metadataProviderName == 0 || metadataID == "" {
			log.Errorf("skipping invalid card '%s'", c.Name)
			continue
		}

		id, err := insertCard(c, metadataID, metadataProviderName, mediaType, sourceType, sourceURI)
		if err != nil {
			log.Errorf("failed to update deprecated media table: %v", err)
			continue
		}

		/*
			i := api.Identify{
				CreatedAt: time.Now().Format(time.RFC3339),
				Media: &api.Media{
					Id:         id,
					Type:       mediaType,
					Metadata:   metadataProviderName,
					MetadataId: metadataID,
					Creator:    api.Media_TRELLO,
					CreatorId:  c.ID,
					Source:     sourceType,
					SourceURI:  sourceURI,
					Status:     0,
				},
			}
			b, err := proto.Marshal(&i)
			if err != nil {
				log.Errorf("failed to create protobuf encoded message for identifier: %v", err)
				continue
			}

			if err := amqpClient.Publish("v1.identify", b); err != nil {
				log.Errorf("failed to publish message to rabbitmq: %v", err)
				continue
			}
		*/

		if err := findEpisodes(mediaType, id, c.Name); err != nil {
			log.Errorf("failed to find existing episodes for this media: %v", err)
		}
	}
}
