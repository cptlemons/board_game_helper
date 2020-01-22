package collection

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

type collectionItem struct {
	ObjectID string `xml:"objectid,attr"`
}

type collection struct {
	Items []collectionItem `xml:"item"`
}

type gameName struct {
	Name string `xml:"value,attr"`
	Type string `xml:"type,attr"`
}

type result struct {
	NumPlayers string `xml:"numplayers,attr"`
	Votes      []struct {
		Num int `xml:"numvotes,attr"`
	} `xml:"result"`
}

type poll struct {
	Name       string   `xml:"name,attr"`
	TotalVotes int      `xml:"totalvotes,attr"`
	Results    []result `xml:"results"`
}

type gameXML struct {
	Names       []gameName `xml:"item>name"`
	PrimaryName string     `xml:"-"`
	Description string     `xml:"item>description"`
	MinPlayers  struct {
		Num int `xml:"value,attr"`
	} `xml:"item>minplayers"`
	MaxPlayers struct {
		Num int `xml:"value,attr"`
	} `xml:"item>maxplayers"`
	Polls []*poll `xml:"item>poll"`
}

type gameJSON struct {
	Score   float64 `json:"average,string"`
	Weight  float64 `json:"avgweight,string"`
	BScore  float64 `json:"baverage,string"`
	Ratings int     `json:"usersrated,string"`
}

type game struct {
	Name       string
	ID         string
	Best       bool
	Rec        bool
	MinPlayers int
	MaxPlayers int
	Score      float64
	Weight     float64
	BScore     float64
	Ratings    int
}

func formWrapper(h http.HandlerFunc, params ...string) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, fmt.Sprintf("bad form values %s", err), http.StatusBadRequest)
			return
		}
		for _, param := range params {
			if len(r.Form[param]) == 0 {
				http.Error(w, "missing "+param, http.StatusBadRequest)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

// Home is the homepage function.
func Home(tpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := tpl.ExecuteTemplate(w, "home.html", nil); err != nil {
			log.Printf("Error executing template: %s", err)
			return
		}
	}
}

type collectionData struct {
	BGGName    string
	NumPlayers int
	Games      []*game
}

// Collection is the Collection page function.
func Collection(tpl *template.Template, client *http.Client) http.HandlerFunc {
	return formWrapper(func(w http.ResponseWriter, r *http.Request) {
		bggName := r.FormValue("bggName")
		if len(bggName) < 4 || len(bggName) > 20 {
			http.Error(w, "bad bgg name param, please provide a name between 4-20 characters", http.StatusBadRequest)
			return
		}
		numPlayers, err := strconv.Atoi(r.FormValue("numPlayers"))
		if err != nil {
			http.Error(w, "bad num players param, please provide a number", http.StatusBadRequest)
			return
		}
		if numPlayers < 1 || numPlayers > 100 {
			http.Error(w, "bad num players param, please provide a number between 1 and 100", http.StatusBadRequest)
			return
		}

		games, err := fetchCollection(client, bggName, numPlayers)
		if err != nil {
			http.Error(w, "unable to get collection information", http.StatusServiceUnavailable)
			log.Printf("%s", err)
			return
		}

		data := collectionData{
			BGGName:    bggName,
			NumPlayers: numPlayers,
			Games:      games,
		}
		if err := tpl.ExecuteTemplate(w, "collection.html", data); err != nil {
			log.Printf("Error executing template: %s", err)
			return
		}
	}, "numPlayers", "bggName")
}

func fetchCollection(client *http.Client, bggName string, numPlayers int) (games []*game, err error) {
	collURL := &url.URL{
		Scheme: "https",
		Host:   "www.boardgamegeek.com",
		Path:   "/xmlapi2/collection",
		RawQuery: url.Values{
			"username":       {bggName},
			"excludesubtype": {"boardgameexpansion"},
			"own":            {"1"},
		}.Encode(),
	}
retry:
	resp, err := client.Get(collURL.String())
	if err != nil {
		return nil, fmt.Errorf("error fetching collection: %s", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusAccepted {
		log.Printf("BGG request accepted, waiting for body")
		time.Sleep(10 * time.Second)
		goto retry
	}

	// TODO: BGG gives 200 on invalid username, write check to let user know they provided invalid name and to try again
	raw, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("Failed to read collection body: %s", err)
	}

	var coll collection
	if err := xml.Unmarshal(raw, &coll); err != nil {
		return nil, fmt.Errorf("Failed to unmarshal XML: %s", err)
	}

	var wg sync.WaitGroup
	allGames := make([]*game, len(coll.Items))
	for i, game := range coll.Items {
		wg.Add(1)
		i, game := i, game // don't capture loop variables
		go func() {
			defer wg.Done()
			g, err := fetchGame(client, game.ObjectID, numPlayers)
			if err != nil {
				log.Printf("warning: unable to fetch game %q info: %s", game.ObjectID, err)
				return
			}
			allGames[i] = g // only safe due to preallocation of array size
		}()
	}
	wg.Wait()
	for _, g := range allGames {
		if g != nil {
			return allGames, nil
		}
	}
	return nil, fmt.Errorf("no valid games found")
}

func fetchGame(client *http.Client, gameID string, numPlayers int) (*game, error) {
	xmlURL := &url.URL{
		Scheme: "https",
		Host:   "www.boardgamegeek.com",
		Path:   "/xmlapi2/thing",
		RawQuery: url.Values{
			"id": {gameID},
		}.Encode(),
	}

	xresp, err := client.Get(xmlURL.String())
	if err != nil {
		return nil, fmt.Errorf("error fetching game xml: %s", err)
	}
	defer xresp.Body.Close()

	if xresp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Bad status code fetching game xml: %s", xresp.Status)
	}

	var gXML gameXML
	if err := xml.NewDecoder(xresp.Body).Decode(&gXML); err != nil {
		return nil, fmt.Errorf("error decoding game xml: %s", err)
	}

	for _, name := range gXML.Names {
		if name.Type == "primary" {
			gXML.PrimaryName = name.Name
			break
		}
	}

	bestAt, recAt, err := gXML.parsePolls(numPlayers)
	if err != nil {
		return nil, fmt.Errorf("error parsing polls: %s", err)
	}

	jsonURL := &url.URL{
		Scheme: "https",
		Host:   "www.boardgamegeek.com",
		Path:   path.Join("/boardgame", url.PathEscape(gameID)),
	}

	jresp, err := client.Get(jsonURL.String())
	if err != nil {
		return nil, fmt.Errorf("error fetching game json: %s", err)
	}
	defer jresp.Body.Close()

	if jresp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Bad status code fetching game json: %s", jresp.Status)
	}
	gJSON, err := jsonDecode(jresp.Body)
	if err != nil {
		return nil, fmt.Errorf("Unable to decode json: %s", err)
	}

	return &game{
		Name:       gXML.PrimaryName,
		ID:         gameID,
		Best:       bestAt,
		Rec:        recAt,
		MinPlayers: gXML.MinPlayers.Num,
		MaxPlayers: gXML.MaxPlayers.Num,
		Score:      gJSON.Score,
		Weight:     gJSON.Weight,
		BScore:     gJSON.BScore,
		Ratings:    gJSON.Ratings,
	}, nil
}

func (gx *gameXML) parsePolls(targetPlayers int) (bestAt, recAt bool, err error) {
	var playerPoll *poll
	for _, poll := range gx.Polls {
		switch poll.Name {
		case "suggested_numplayers":
			playerPoll = poll
			/*case "suggested_playerage":
				agePoll = poll
			case "language_dependence":
				langPoll = poll
			*/
		}
	}
	// TODO: check votes and defer to min/max players if <n
	if playerPoll != nil {
		for _, playerCount := range playerPoll.Results {
			bestVotes, recVotes, nayVotes := playerCount.Votes[0].Num, playerCount.Votes[1].Num, playerCount.Votes[2].Num

			// BGG can return n+ which is taken here as 1 more than the max number of players on the box
			numPlayers, err := strconv.Atoi(strings.TrimSuffix(playerCount.NumPlayers, "+"))
			if err != nil {
				return false, false, fmt.Errorf("Failed to convert numPlayers string to int: %s", err)
			}
			if bestVotes+recVotes <= nayVotes {
				continue
			}
			if bestVotes > recVotes {
				bestAt = true
			}
			if strings.HasSuffix(playerCount.NumPlayers, "+") {
				if numPlayers*2 >= targetPlayers {
					return bestAt, !bestAt, nil
				}
			}
			if numPlayers == targetPlayers {
				return bestAt, !bestAt, nil
			}
		}
	}
	return false, false, nil
}

func jsonDecode(r io.Reader) (*gameJSON, error) {
	htmlRaw, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("Failed to read body: %s", err)
	}

	needle := []byte("GEEK.geekitemPreload")
	start := bytes.Index(htmlRaw, needle)
	if start < 0 {
		return nil, fmt.Errorf("Couldn't find GEEK.geekitemPreload in htmlRaw")
	}
	start += len(needle)

	preload := htmlRaw[start:]
	brace := bytes.IndexByte(preload, '{')
	if brace < 0 {
		return nil, fmt.Errorf("Couldn't find the first brace in preloaded data")
	}
	preload = preload[brace:]

	var data struct{ Item struct{ Stats gameJSON } }
	if err := json.NewDecoder(bytes.NewReader(preload)).Decode(&data); err != nil {
		return nil, fmt.Errorf("Failed to parse json")
	}
	return &data.Item.Stats, nil
}
