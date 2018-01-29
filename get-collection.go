package main

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"time"
)

func main() {
	getCollection("CPT_Lemons")
}

func getCollection(bggName string) {
	var json []byte
	url := fmt.Sprintf("https://www.boardgamegeek.com/xmlapi2/collection?username=%s&excludesubtype=boardgameexpansion&own=1", bggName)
	for {
		resp, err := http.Get(url)
		if err != nil {
			fmt.Println(err)
		}
		if resp.StatusCode == http.StatusAccepted {
			fmt.Println("BGG request accepted, waiting for body")
			time.Sleep(10 * time.Second)
			continue
		}
		js, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			fmt.Println(err)
		}
		json = js
		fmt.Printf("%s\n", json)
	}
}
