package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-yaml/yaml"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/pkg/errors"
	"github.com/totegamma/httpsig"
	xhtml "golang.org/x/net/html"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	apmiddleware "github.com/concrnt/ccworld-ap-bridge/middleware"
	capstore "github.com/concrnt/ccworld-ap-bridge/store"
	"github.com/concrnt/ccworld-ap-bridge/types"
	"github.com/concrnt/ccworld-ap-bridge/world"
	ccclient "github.com/totegamma/concurrent/client"
	"github.com/totegamma/concurrent/core"
	commitStore "github.com/totegamma/concurrent/x/store"
)

const (
	UserAgent = "ccworld-ap-relay/0.1"
)

type Config struct {
	FQDN         string   `yaml:"fqdn"`
	PrivateKey   string   `yaml:"private_key"`
	PublicKey    string   `yaml:"public_key"`
	Destinations []string `yaml:"destinations"`
	Source       string   `yaml:"source"`
	ProxyPriv    string   `yaml:"proxy_priv"`
	ProxyCCID    string   `yaml:"proxy_ccid"`
	ProxyHost    string   `yaml:"proxy_host"`
	Dsn          string   `yaml:"dsn"`
}

var config Config
var client ccclient.Client
var store *capstore.Store
var domain string

func main() {

	log.Println("Starting ccworld-ap-relay")

	f, err := os.Open("/etc/ccworld-ap-relay.yaml")
	if err != nil {
		panic(err)
	}
	defer f.Close()

	err = yaml.NewDecoder(f).Decode(&config)
	if err != nil {
		panic(err)
	}

	log.Println("Config Loaded! FQDN:", config.FQDN)

	db, err := gorm.Open(postgres.Open(config.Dsn), &gorm.Config{
		TranslateError: true,
	})
	if err != nil {
		panic("failed to connect database")
	}

	client = ccclient.NewClient()
	store = capstore.NewStore(db)

	e := echo.New()
	e.Use(middleware.Recover())
	e.Use(middleware.Logger())
	e.Binder = &apmiddleware.Binder{}

	e.GET("/", func(c echo.Context) error {
		return c.String(http.StatusOK, "Hello, World!")
	})

	e.GET("/.well-known/nodeinfo", WellKnownNodeInfo)
	e.GET("/nodeinfo/2.0", NodeInfo)
	e.GET("/actor", AcctRelay)

	e.POST("/inbox", Inbox)

	log.Println("subscribing to", config.Source)
	go SubscribeTimeline(config.Source)

	port := ":8000"
	envport := os.Getenv("PORT")
	if envport != "" {
		port = ":" + envport
	}

	log.Println("Starting server on", port)
	e.Logger.Fatal(e.Start(port))
}

func SubscribeTimeline(id string) {

	ctx := context.Background()

	split := strings.Split(id, "@")
	if len(split) != 2 {
		panic("Invalid source ID")
	}

	domain := split[1]

	u := url.URL{Scheme: "wss", Host: domain, Path: "/api/v1/timelines/realtime"}
	dialer := websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second

	c, _, err := dialer.Dial(u.String(), nil)
	if err != nil {
		log.Println("websocket.Dial:", err)
		panic(err)
	}

	c.WriteJSON(map[string]any{
		"type":     "listen",
		"channels": []string{id},
	})

	fmt.Println("Subscribed to", id)

	pingTicker := time.NewTicker(10 * time.Second)
	defer pingTicker.Stop()

	go func() {
		for range pingTicker.C {
			err := c.WriteMessage(websocket.PingMessage, nil)
			if err != nil {
				log.Println("websocket.PingMessage:", err)
				continue
			}
		}
	}()

	for {
		_, message, err := c.ReadMessage()
		if err != nil {
			log.Println("websocket.ReadMessage:", err)
			panic(err)
		}

		var event core.Event
		err = json.Unmarshal(message, &event)
		if err != nil {
			log.Println("json.Unmarshal:", err)
			continue
		}

		if event.Timeline != id {
			continue
		}

		if event.Item == nil {
			continue
		}

		if len(event.Item.ResourceID) == 0 {
			log.Println("ResourceID is empty")
			jsonPrint("Event", event)
			continue
		}

		if event.Item.ResourceID[0] != 'm' {
			continue
		}

		entity, err := client.GetEntity(ctx, domain, event.Item.Owner, nil)
		if err != nil {
			log.Println("GetEntity:", err)
			continue
		}

		apURL := "https://" + entity.Domain + "/ap/note/" + event.Item.ResourceID
		err = Announce(ctx, apURL)
		if err != nil {
			log.Println("Announce:", err)
			continue
		}

		jsonPrint("Event", event)
	}

}

func Announce(ctx context.Context, objectURL string) error {

	announce := types.ApObject{
		Context: []string{"https://www.w3.org/ns/activitystreams"},
		Type:    "Announce",
		ID:      "https://" + config.FQDN + "/note/" + url.PathEscape(objectURL) + "/activity",
		Actor:   "https://" + config.FQDN + "/actor",
		Content: "",
		Object:  objectURL,
	}

	announceBytes, err := json.Marshal(announce)
	if err != nil {
		log.Println("api/handler/announce json.Marshal:", err)
		return err
	}

	for _, dest := range config.Destinations {
		err = PostToInbox(ctx, dest, announceBytes)
		if err != nil {
			log.Println("api/handler/announce PostToInbox:", err)
			continue
		}
	}

	return nil
}

func Inbox(c echo.Context) error {
	ctx := c.Request().Context()

	bytes, err := io.ReadAll(c.Request().Body)
	if err != nil {
		log.Println("api/handler/inbox io.ReadAll:", err)
		return c.String(http.StatusBadRequest, "Invalid request body")
	}

	object, err := types.LoadAsRawApObj(bytes)
	if err != nil {
		log.Println("api/handler/inbox LoadAsRawApObj:", err)
		return c.String(http.StatusBadRequest, "Invalid request body")
	}

	switch object.MustGetString("type") {
	case "Follow":
		requester, err := FetchPerson(ctx, object.MustGetString("actor"))
		if err != nil {
			log.Println("ap/service/inbox/follow FetchPerson:", err)
			return c.JSON(http.StatusOK, echo.Map{"error": "FetchPerson Error"})
		}

		accept := types.ApObject{
			Context: "https://www.w3.org/ns/activitystreams",
			ID:      "https://" + config.FQDN + "/actor/follows/" + url.PathEscape(requester.MustGetString("id")),
			Type:    "Accept",
			Actor:   "https://" + config.FQDN + "/actor",
			Object:  object,
		}

		acceptBytes, err := json.Marshal(accept)
		if err != nil {
			log.Println("ap/service/inbox/follow json.Marshal:", err)
			return c.JSON(http.StatusOK, echo.Map{"error": "json.Marshal Error"})
		}

		err = PostToInbox(ctx, requester.MustGetString("inbox"), acceptBytes)
		if err != nil {
			log.Println("ap/service/inbox/follow PostToInbox:", err)
			return c.JSON(http.StatusOK, echo.Map{"error": "PostToInbox Error"})
		}

		return c.JSON(http.StatusOK, echo.Map{"success": "Follow Accepted"})

	case "Create":
		createObject, ok := object.GetRaw("object")
		if !ok {
			log.Println("ap/service/inbox/create Invalid Create Object")
			return c.JSON(http.StatusOK, echo.Map{"error": "Invalid Create Object"})
		}
		createType, ok := createObject.GetString("type")
		if !ok {
			log.Println("ap/service/inbox/create Invalid Create Object")
			return c.JSON(http.StatusOK, echo.Map{"error": "Invalid Create Object"})
		}
		createID, ok := createObject.GetString("id")
		if !ok {
			log.Println("ap/service/inbox/create Invalid Create Object")
			return c.JSON(http.StatusOK, echo.Map{"error": "Invalid Create Object"})
		}
		switch createType {
		case "Note":
			// check if the note is already exists
			_, err := store.GetApObjectReferenceByCcObjectID(ctx, createID)
			if err == nil {
				// already exists
				log.Println("ap/service/inbox/create note already exists")
				return c.JSON(http.StatusOK, echo.Map{"error": "Note already exists"})
			}

			// preserve reference
			err = store.CreateApObjectReference(ctx, types.ApObjectReference{
				ApObjectID: createID,
				CcObjectID: "",
			})

			if err != nil {
				log.Println("ap/service/inbox/create CreateApObjectReference", err)
				return c.JSON(http.StatusOK, echo.Map{"error": "CreateApObjectReference Error"})
			}

			person, err := FetchPerson(ctx, object.MustGetString("actor"))
			if err != nil {
				log.Println("ap/service/inbox/create FetchPerson", err)
				return c.JSON(http.StatusOK, echo.Map{"error": "FetchPerson Error"})
			}

			created, err := NoteToMessage(ctx, createObject, person, []string{config.Source})
			if err != nil {
				log.Println("ap/service/inbox/create NoteToMessage", err)
				return c.JSON(http.StatusOK, echo.Map{"error": "NoteToMessage Error"})
			}

			// save reference
			err = store.UpdateApObjectReference(ctx, types.ApObjectReference{
				ApObjectID: createID,
				CcObjectID: created.ID,
			})
			if err != nil {
				log.Println("ap/service/inbox/create UpdateApObjectReference", err)
			}

			return c.JSON(http.StatusOK, echo.Map{"success": "Note Created"})
		default:
			// print request body
			b, err := json.Marshal(object)
			if err != nil {
				log.Println("ap/service/inbox/create Marshal", err)
				return c.JSON(http.StatusOK, echo.Map{"error": "Marshal Error"})
			}
			log.Println("Unhandled Create Object", string(b))
			return c.JSON(http.StatusOK, echo.Map{"error": "Unhandled Create Object"})
		}

	case "Delete":
		deleteObject, ok := object.GetRaw("object")
		if !ok {
			jsonPrint("Delete Object", object)
			return c.JSON(http.StatusOK, echo.Map{"error": "Invalid Delete Object"})
		}
		deleteID, ok := deleteObject.GetString("id")
		if !ok {
			jsonPrint("Delete Object", object)
			return c.JSON(http.StatusOK, echo.Map{"error": "Invalid Delete Object"})
		}

		deleteRef, err := store.GetApObjectReferenceByApObjectID(ctx, deleteID)
		if err != nil {
			log.Println("ap/service/inbox/delete GetApObjectReferenceByApObjectID", err)
			return c.JSON(http.StatusOK, echo.Map{"error": "GetApObjectReferenceByApObjectID Error"})
		}

		doc := core.DeleteDocument{
			DocumentBase: core.DocumentBase[any]{
				Signer:   config.ProxyCCID,
				Type:     "delete",
				SignedAt: time.Now(),
			},
			Target: deleteRef.CcObjectID,
		}

		document, err := json.Marshal(doc)
		if err != nil {
			log.Println("ap/service/inbox/delete Marshal", err)
			return c.JSON(http.StatusOK, echo.Map{"error": "Marshal Error"})
		}

		signatureBytes, err := core.SignBytes(document, config.ProxyPriv)
		if err != nil {
			log.Println("ap/service/inbox/delete SignBytes", err)
			return c.JSON(http.StatusOK, echo.Map{"error": "SignBytes Error"})
		}

		signature := hex.EncodeToString(signatureBytes)

		opt := commitStore.CommitOption{
			IsEphemeral: true,
		}

		option, err := json.Marshal(opt)
		if err != nil {
			log.Println("ap/service/inbox/delete Marshal", err)
			return c.JSON(http.StatusOK, echo.Map{"error": "Marshal Error"})
		}

		commitObj := core.Commit{
			Document:  string(document),
			Signature: string(signature),
			Option:    string(option),
		}

		commit, err := json.Marshal(commitObj)
		if err != nil {
			log.Println("ap/service/inbox/delete Marshal", err)
			return c.JSON(http.StatusOK, echo.Map{"error": "Marshal Error"})
		}

		_, err = client.Commit(ctx, config.ProxyHost, string(commit), nil, nil)
		if err != nil {
			log.Println("ap/service/inbox/delete Commit", err)
			return c.JSON(http.StatusOK, echo.Map{"error": "Commit Error"})
		}

		err = store.DeleteApObjectReference(ctx, deleteRef.ApObjectID)
		if err != nil {
			log.Println("ap/service/inbox/delete DeleteApObjectReference", err)
			return c.JSON(http.StatusOK, echo.Map{"error": "DeleteApObjectReference Error"})
		}
		return c.JSON(http.StatusOK, echo.Map{"success": "Note Deleted"})

	case "Announce":
		announceObject, ok := object.GetString("object") //object.Object.(string)
		if !ok {
			log.Println("ap/service/inbox/announce Invalid Announce Object")
			return c.JSON(http.StatusOK, echo.Map{"error": "Invalid Announce Object"})
		}
		// check if the note is already exists
		_, err := store.GetApObjectReferenceByCcObjectID(ctx, object.MustGetString("id"))
		if err == nil {
			// already exists
			log.Println("ap/service/inbox/announce note already exists")
			return c.JSON(http.StatusOK, echo.Map{"error": "Note already exists"})
		}

		// preserve reference
		err = store.CreateApObjectReference(ctx, types.ApObjectReference{
			ApObjectID: object.MustGetString("id"),
			CcObjectID: "",
		})

		if err != nil {
			log.Println("ap/service/inbox/announce CreateApObjectReference", err)
			return c.JSON(http.StatusOK, echo.Map{"error": "CreateApObjectReference Error"})
		}

		person, err := FetchPerson(ctx, object.MustGetString("actor"))
		if err != nil {
			log.Println("ap/service/inbox/announce FetchPerson", err)
			return c.JSON(http.StatusOK, echo.Map{"error": "FetchPerson Error"})
		}

		var sourceMessage core.Message

		// import note
		existing, err := store.GetApObjectReferenceByApObjectID(ctx, announceObject)
		if err == nil {
			message, err := client.GetMessage(ctx, config.ProxyHost, existing.CcObjectID, nil)
			if err == nil {
				sourceMessage = message
			}
			log.Println("message not found: ", existing.CcObjectID, err)
			store.DeleteApObjectReference(ctx, announceObject)
		} else {
			// fetch note
			note, err := FetchNote(ctx, announceObject)
			if err != nil {
				log.Println("ap/service/inbox/announce FetchNote", err)
				return c.JSON(http.StatusOK, echo.Map{"error": "FetchNote Error"})
			}

			// save person
			person, err := FetchPerson(ctx, note.MustGetString("attributedTo"))
			if err != nil {
				log.Println("ap/service/inbox/announce FetchPerson", err)
				return c.JSON(http.StatusOK, echo.Map{"error": "FetchPerson Error"})
			}

			// save note as concurrent message
			sourceMessage, err = NoteToMessage(ctx, note, person, []string{world.UserHomeStream + "@" + config.ProxyCCID})
			if err != nil {
				log.Println("ap/service/inbox/announce NoteToMessage", err)
				return c.JSON(http.StatusOK, echo.Map{"error": "NoteToMessage Error"})
			}

			// save reference
			err = store.CreateApObjectReference(ctx, types.ApObjectReference{
				ApObjectID: announceObject,
				CcObjectID: sourceMessage.ID,
			})
			if err != nil {
				log.Println("ap/service/inbox/announce CreateApObjectReference", err)
				return c.JSON(http.StatusOK, echo.Map{"error": "CreateApObjectReference Error"})
			}
		}

		username := person.MustGetString("name")
		if len(username) == 0 {
			username = person.MustGetString("preferredUsername")
		}

		doc := core.MessageDocument[world.RerouteMessage]{
			DocumentBase: core.DocumentBase[world.RerouteMessage]{
				Signer:   config.ProxyCCID,
				Type:     "message",
				Schema:   world.RerouteMessageSchema,
				SignedAt: time.Now(),
				Body: world.RerouteMessage{
					RerouteMessageID:     sourceMessage.ID,
					RerouteMessageAuthor: sourceMessage.Author,
					Body:                 object.MustGetString("content"),
					ProfileOverride: &world.ProfileOverride{
						Username:    username,
						Avatar:      person.MustGetString("icon.url"),
						Description: person.MustGetString("summary"),
						Link:        person.MustGetString("actor"),
					},
				},
				Meta: map[string]any{
					"apActor":          person.MustGetString("url"),
					"apObject":         object.MustGetString("id"),
					"apPublisherInbox": person.MustGetString("inbox"),
				},
			},
			Timelines: []string{config.Source},
		}

		document, err := json.Marshal(doc)
		if err != nil {
			log.Println("ap/service/inbox/announce Marshal", err)
			return c.JSON(http.StatusOK, echo.Map{"error": "Marshal Error"})
		}

		signatureBytes, err := core.SignBytes(document, config.ProxyPriv)
		if err != nil {
			log.Println("ap/service/inbox/announce SignBytes", err)
			return c.JSON(http.StatusOK, echo.Map{"error": "SignBytes Error"})
		}

		signature := hex.EncodeToString(signatureBytes)

		opt := commitStore.CommitOption{
			IsEphemeral: true,
		}

		option, err := json.Marshal(opt)
		if err != nil {
			log.Println("ap/service/inbox/announce Marshal", err)
			return c.JSON(http.StatusOK, echo.Map{"error": "Marshal Error"})
		}

		commitObj := core.Commit{
			Document:  string(document),
			Signature: string(signature),
			Option:    string(option),
		}

		commit, err := json.Marshal(commitObj)
		if err != nil {
			log.Println("ap/service/inbox/announce Marshal", err)
			return c.JSON(http.StatusOK, echo.Map{"error": "Marshal Error"})
		}

		var created core.ResponseBase[core.Message]
		_, err = client.Commit(ctx, config.ProxyHost, string(commit), &created, nil)
		if err != nil {
			log.Println("ap/service/inbox/announce Commit", err)
			return c.JSON(http.StatusOK, echo.Map{"error": "Commit Error"})
		}

		// save reference
		err = store.UpdateApObjectReference(ctx, types.ApObjectReference{
			ApObjectID: object.MustGetString("id"),
			CcObjectID: created.Content.ID,
		})

		if err != nil {
			log.Println("ap/service/inbox/announce UpdateApObjectReference", err)
		}

		return c.JSON(http.StatusOK, echo.Map{"success": "Announce Created"})

	default:
		// print request body
		jsonPrint("Unhandled Activitypub Object", object)
		return c.JSON(http.StatusOK, echo.Map{"error": "Unhandled Activitypub Object"})
	}
}

func jsonPrint(title string, v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("----- : " + title + " : -----")
	fmt.Println(string(b))
	fmt.Println("--------------------------------")
}

func FetchPerson(ctx context.Context, actor string) (*types.RawApObj, error) {

	req, err := http.NewRequest("GET", actor, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/activity+json")
	req.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Host", req.URL.Host)
	client := new(http.Client)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	person, err := types.LoadAsRawApObj(body)
	if err != nil {
		log.Println(err)
		return person, err
	}

	return person, nil
}

func PostToInbox(ctx context.Context, inbox string, objectBytes []byte) error {

	req, err := http.NewRequest("POST", inbox, bytes.NewBuffer(objectBytes))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/activity+json")
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	req.Header.Set("Host", req.URL.Host)
	client := new(http.Client)

	prefs := []httpsig.Algorithm{httpsig.RSA_SHA256}
	digestAlgorithm := httpsig.DigestSha256
	headersToSign := []string{httpsig.RequestTarget, "date", "digest", "host"}
	signer, _, err := httpsig.NewSigner(prefs, digestAlgorithm, headersToSign, httpsig.Signature, 0)
	if err != nil {
		log.Println(err)
		return err
	}

	block, _ := pem.Decode([]byte(config.PrivateKey))
	if block == nil {
		return fmt.Errorf("failed to decode PEM block containing private key")
	}

	priv, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return err
	}

	err = signer.SignRequest(priv, "https://"+config.FQDN+"/actor#main-key", req, objectBytes)
	if err != nil {
		log.Println(err)
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		log.Println(err)
		return err
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Println(err)
	}
	log.Printf("POST %s [%d]: %s", inbox, resp.StatusCode, string(body))

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("error posting to inbox: %d", resp.StatusCode)
	}

	defer resp.Body.Close()

	return nil
}

func WellKnownNodeInfo(c echo.Context) error {
	c.Response().Header().Set("Content-Type", "application/json")
	return c.JSON(http.StatusOK, types.WellKnown{
		Links: []types.WellKnownLink{
			{
				Rel:  "http://nodeinfo.diaspora.software/ns/schema/2.0",
				Href: "https://" + config.FQDN + "/nodeinfo/2.0",
			},
		},
	})
}

func NodeInfo(c echo.Context) error {
	c.Response().Header().Set("Content-Type", "application/json")
	return c.JSON(http.StatusOK, types.NodeInfo{
		Version: "2.0",
		Software: types.NodeInfoSoftware{
			Name:    "ccworld-ap-relay",
			Version: "0.1",
		},
		Protocols: []string{
			"activitypub",
		},
		OpenRegistrations: false,
		Metadata: types.NodeInfoMetadata{
			NodeName:        "ccworld-ap-relay",
			NodeDescription: "relay service for ccworld",
			Maintainer: types.NodeInfoMetadataMaintainer{
				Name: "totegamma",
			},
			ThemeColor: "#000000",
		},
	})
}

func AcctRelay(c echo.Context) error {
	c.Response().Header().Set("Content-Type", "application/activity+json")
	return c.JSON(http.StatusOK, types.ApObject{
		Context:           "https://www.w3.org/ns/activitystreams",
		Type:              "Application",
		PreferredUsername: "relay",
		ID:                "https://" + config.FQDN + "/actor",
		Inbox:             "https://" + config.FQDN + "/actor/inbox",
		Outbox:            "https://" + config.FQDN + "/actor/outbox",
		Following:         "https://" + config.FQDN + "/actor/following",
		Followers:         "https://" + config.FQDN + "/actor/followers",
		PublicKey: &types.Key{
			ID:           "https://" + config.FQDN + "/actor#main-key",
			Type:         "Key",
			Owner:        "https://" + config.FQDN + "/actor",
			PublicKeyPem: config.PublicKey,
		},
	})
}

func htmlToMarkdown(r io.Reader) (string, error) {
	doc, err := xhtml.Parse(r)
	if err != nil {
		return "", err
	}

	// traverse はノード n を受け取り、変換後の文字列を返す再帰関数です。
	var traverse func(n *xhtml.Node) string
	traverse = func(n *xhtml.Node) string {
		var result strings.Builder

		switch n.Type {
		case xhtml.TextNode:
			result.WriteString(n.Data)
		case xhtml.ElementNode:
			switch n.Data {
			case "a":
				var href string
				for _, attr := range n.Attr {
					if attr.Key == "href" {
						href = attr.Val
						break
					}
				}
				result.WriteString("[")
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					result.WriteString(traverse(c))
				}
				result.WriteString(fmt.Sprintf("](%s)", href))
			case "p":
				result.WriteString("\n\n")
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					result.WriteString(traverse(c))
				}
			case "br":
				result.WriteString("\n")
			default:
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					result.WriteString(traverse(c))
				}
			}
		default:
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				result.WriteString(traverse(c))
			}
		}
		return result.String()
	}

	return traverse(doc), nil
}

func NoteToMessage(ctx context.Context, object *types.RawApObj, person *types.RawApObj, destStreams []string) (core.Message, error) {

	content, ok := object.GetString("_misskey_content")
	if !ok {
		rawcontent := object.MustGetString("content")
		if rawcontent != "" {
			var err error
			content, err = htmlToMarkdown(strings.NewReader(rawcontent))
			if err != nil {
				fmt.Println("html to markdown error", err)
				content = rawcontent
			}
			content = strings.Trim(content, "\n")
		}
	}

	tags, _ := object.GetRawSlice("tag")
	var emojis map[string]world.Emoji = make(map[string]world.Emoji)
	for _, tag := range tags {
		if tag.MustGetString("type") == "Emoji" {
			name := strings.Trim(tag.MustGetString("name"), ":")
			emojis[name] = world.Emoji{
				ImageURL: tag.MustGetString("icon.url"),
			}
		}
	}

	if len(content) == 0 {
		return core.Message{}, errors.New("empty note")
	}

	if len(content) > 4096 {
		return core.Message{}, errors.New("note too long")
	}

	contentWithImage := content
	for _, attachment := range object.MustGetRawSlice("attachment") {
		if attachment.MustGetString("type") == "document" {
			contentWithImage += "\n\n![image](" + attachment.MustGetString("url") + ")"
		}
	}

	if object.MustGetBool("sensitive") {
		summary := "CW"
		if object.MustGetString("summary") != "" {
			summary = object.MustGetString("summary")
		}
		content = "<details>\n<summary>" + summary + "</summary>\n" + content + "\n</details>"
		contentWithImage = "<details>\n<summary>" + summary + "</summary>\n" + contentWithImage + "\n</details>"
	}

	username := person.MustGetString("name")
	if len(username) == 0 {
		username = person.MustGetString("preferredUsername")
	}

	date, err := time.Parse(time.RFC3339, object.MustGetString("published"))
	if err != nil {
		date = time.Now()
	}

	var policy = ""
	var policyParams = ""

	var document []byte
	if object.MustGetString("inReplyTo") == "" {

		media := []world.Media{}
		for _, attachment := range object.MustGetRawSlice("attachment") {
			flag := ""
			if attachment.MustGetBool("sensitive") || object.MustGetBool("sensitive") {
				flag = "sensitive"
			}
			media = append(media, world.Media{
				MediaURL:  attachment.MustGetString("url"),
				MediaType: attachment.MustGetString("mediaType"),
				Flag:      flag,
			})
		}

		if len(object.MustGetRawSlice("attachment")) > 0 {
			doc := core.MessageDocument[world.MediaMessage]{
				DocumentBase: core.DocumentBase[world.MediaMessage]{
					Signer: config.ProxyCCID,
					Type:   "message",
					Schema: world.MediaMessageSchema,
					Body: world.MediaMessage{
						Body: content,
						ProfileOverride: &world.ProfileOverride{
							Username:    username,
							Avatar:      person.MustGetString("icon.url"),
							Description: person.MustGetString("summary"),
							Link:        person.MustGetString("url"),
						},
						Medias: &media,
						Emojis: &emojis,
					},
					Meta: map[string]any{
						"apActor":          person.MustGetString("url"),
						"apObjectRef":      object.MustGetString("id"),
						"apPublisherInbox": person.MustGetString("inbox"),
					},
					SignedAt:     date,
					Policy:       policy,
					PolicyParams: policyParams,
				},
				Timelines: destStreams,
			}
			document, err = json.Marshal(doc)
			if err != nil {
				return core.Message{}, errors.Wrap(err, "json marshal error")
			}
		} else {
			doc := core.MessageDocument[world.MarkdownMessage]{
				DocumentBase: core.DocumentBase[world.MarkdownMessage]{
					Signer: config.ProxyCCID,
					Type:   "message",
					Schema: "https://schema.concrnt.world/m/mfm.json",
					Body: world.MarkdownMessage{
						Body: content,
						ProfileOverride: &world.ProfileOverride{
							Username:    username,
							Avatar:      person.MustGetString("icon.url"),
							Description: person.MustGetString("summary"),
							Link:        person.MustGetString("url"),
						},
						Emojis: &emojis,
					},
					Meta: map[string]any{
						"apActor":          person.MustGetString("url"),
						"apObjectRef":      object.MustGetString("id"),
						"apPublisherInbox": person.MustGetString("inbox"),
					},
					SignedAt:     date,
					Policy:       policy,
					PolicyParams: policyParams,
				},
				Timelines: destStreams,
			}
			document, err = json.Marshal(doc)
			if err != nil {
				return core.Message{}, errors.Wrap(err, "json marshal error")
			}
		}

	} else {

		var ReplyToMessageID string
		var ReplyToMessageAuthor string

		if strings.HasPrefix(object.MustGetString("inReplyTo"), "https://"+config.FQDN+"/ap/note/") {
			replyToMessageID := strings.TrimPrefix(object.MustGetString("inReplyTo"), "https://"+config.FQDN+"/ap/note/")
			message, err := client.GetMessage(ctx, config.FQDN, replyToMessageID, nil)
			if err != nil {
				return core.Message{}, errors.Wrap(err, "message not found")
			}
			ReplyToMessageID = message.ID
			ReplyToMessageAuthor = message.Author
		} else {
			ref, err := store.GetApObjectReferenceByApObjectID(ctx, object.MustGetString("inReplyTo"))
			if err != nil {
				return core.Message{}, errors.Wrap(err, "object not found")
			}
			ReplyToMessageID = ref.CcObjectID
			ReplyToMessageAuthor = config.ProxyCCID
		}

		doc := core.MessageDocument[world.ReplyMessage]{
			DocumentBase: core.DocumentBase[world.ReplyMessage]{
				Signer: config.ProxyCCID,
				Type:   "message",
				Schema: world.ReplyMessageSchema,
				Body: world.ReplyMessage{
					Body: contentWithImage,
					ProfileOverride: &world.ProfileOverride{
						Avatar:      person.MustGetString("icon.url"),
						Description: person.MustGetString("summary"),
						Link:        person.MustGetString("url"),
					},
					Emojis:               &emojis,
					ReplyToMessageID:     ReplyToMessageID,
					ReplyToMessageAuthor: ReplyToMessageAuthor,
				},
				Meta: map[string]any{
					"apActor":          person.MustGetString("url"),
					"apObjectRef":      object.MustGetString("id"),
					"apPublisherInbox": person.MustGetString("inbox"),
				},
				SignedAt:     date,
				Policy:       policy,
				PolicyParams: policyParams,
			},
			Timelines: destStreams,
		}
		document, err = json.Marshal(doc)
		if err != nil {
			return core.Message{}, errors.Wrap(err, "json marshal error")
		}
	}

	signatureBytes, err := core.SignBytes(document, config.ProxyPriv)
	if err != nil {
		return core.Message{}, errors.Wrap(err, "sign error")
	}

	signature := hex.EncodeToString(signatureBytes)

	opt := commitStore.CommitOption{
		IsEphemeral: true,
	}

	option, err := json.Marshal(opt)
	if err != nil {
		return core.Message{}, errors.Wrap(err, "json marshal error")
	}

	commitObj := core.Commit{
		Document:  string(document),
		Signature: string(signature),
		Option:    string(option),
	}

	commit, err := json.Marshal(commitObj)
	if err != nil {
		return core.Message{}, errors.Wrap(err, "json marshal error")
	}

	var created core.ResponseBase[core.Message]
	_, err = client.Commit(ctx, config.ProxyHost, string(commit), &created, nil)
	if err != nil {
		return core.Message{}, err
	}

	return created.Content, nil
}

func FetchNote(ctx context.Context, noteID string) (*types.RawApObj, error) {

	req, err := http.NewRequest("GET", noteID, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/activity+json")
	req.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Host", req.URL.Host)
	client := new(http.Client)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	note, err := types.LoadAsRawApObj(body)
	if err != nil {
		return note, err
	}

	return note, nil
}
