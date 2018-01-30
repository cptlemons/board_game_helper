package collections

import (
	"encoding/xml"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/kylelemons/godebug/pretty"

	"google.golang.org/appengine"
	"google.golang.org/appengine/datastore"
	"google.golang.org/appengine/taskqueue"
	"google.golang.org/appengine/urlfetch"
)

func init() {
	http.HandleFunc("/list", getCollection)
	http.HandleFunc("/loadgame", loadGame)
}

type Item struct {
	ObjectID int `xml:"objectid,attr"`
}

type Collection struct {
	Items     []Item    `xml:"item"`
	FetchTime time.Time `xml:"-"`
}

type Game struct {
	FetchTime time.Time `xml:"-"`
}

func getCollection(w http.ResponseWriter, r *http.Request) {
	ctx := appengine.NewContext(r)
	bggName := r.FormValue("bggName")
	var raw []byte
	collURL := fmt.Sprintf("https://www.boardgamegeek.com/xmlapi2/collection?username=%s&excludesubtype=boardgameexpansion&own=1", bggName)

retry:
	resp, err := urlfetch.Client(ctx).Get(collURL)
	if err != nil {
		log.Fatalf("Failed to download url: %s", err)
	}
	if resp.StatusCode == http.StatusAccepted {
		fmt.Println("BGG request accepted, waiting for body")
		time.Sleep(10 * time.Second)
		goto retry
	}
	// TODO: BGG gives 200 on invalid username, write check to let user know they provided invalid name and to try again
	js, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("Failed to read body: %s", err)
	}
	raw = js
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "%s\n", raw)

	var coll Collection
	if err := xml.Unmarshal(raw, &coll); err != nil {
		log.Fatalf("Failed to unmarshal XML: %s", err)
	}
	coll.FetchTime = time.Now()
	pretty.Fprint(w, coll)

	key := datastore.NewKey(ctx, "Collection", bggName, 0, nil)

	if _, err := datastore.Put(ctx, key, &coll); err != nil {
		log.Fatalf("Failed to store user collection: %s", err)
	}

	var gameKeys []*datastore.Key
	for _, item := range coll.Items {
		gameKeys = append(gameKeys, datastore.NewKey(ctx, "Game", "", int64(item.ObjectID), nil))
	}

	earliestGame := time.Now().Add(-7 * 24 * time.Hour)
	games := make([]Game, len(gameKeys))
	datastore.GetMulti(ctx, gameKeys, games)
	// We don't need error handling because we expect some of these to fail -Kyle 29/01/2018
	var gameTasks []*taskqueue.Task
	for i := range games {
		game := games[i]
		if game.FetchTime.After(earliestGame) {
			continue
		}
		fmt.Fprintf(w, "fetching game %d\n", coll.Items[i].ObjectID)
		gameTasks = append(gameTasks, taskqueue.NewPOSTTask("/loadgame", url.Values{
			"gameID": {fmt.Sprint(coll.Items[i].ObjectID)},
		}))
	}
	if _, err := taskqueue.AddMulti(ctx, gameTasks, ""); err != nil {
		log.Fatalf("Failed to queue game fetch tasks: %s", err)
	}
}

func loadGame(w http.ResponseWriter, r *http.Request) {
	//ctx := appengine.NewContext(r)
	gameID := r.FormValue("gameID")

	fmt.Println(gameID)
}
