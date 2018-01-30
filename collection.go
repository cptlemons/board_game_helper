package collections

import (
	"encoding/xml"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"time"

	"github.com/kylelemons/godebug/pretty"

	"google.golang.org/appengine"
	"google.golang.org/appengine/datastore"
	"google.golang.org/appengine/urlfetch"
)

func init() {
	http.HandleFunc("/list", getCollection)
}

type Item struct {
	ObjectID int `xml:"objectid,attr"`
}

type Collection struct {
	Items     []Item    `xml:"item"`
	FetchTime time.Time `xml:"-"`
}

func getCollection(w http.ResponseWriter, r *http.Request) {
	ctx := appengine.NewContext(r)
	bggName := r.FormValue("bggName")
	var raw []byte
	url := fmt.Sprintf("https://www.boardgamegeek.com/xmlapi2/collection?username=%s&excludesubtype=boardgameexpansion&own=1", bggName)

retry:
	resp, err := urlfetch.Client(ctx).Get(url)
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
}
