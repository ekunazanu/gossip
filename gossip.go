package main

import (
	"encoding/gob"
	"encoding/json"
	"fmt"
	"math"
	"math/bits"
	"net/http"
	"os"
	"time"
)

type (
	TopElements struct {
		Count   uint64    `json:"count"`
		Element [128]byte `json:"element"`
	}

	CMSketch      [131072]uint64  // w = 2^15 d = 4
	HLLSketch     [2048]uint8     // b = 2048
	DSTSketch     [256]uint64     // distribution sketch
	Top16Elements [16]TopElements // only store elements for top 16 elements

	SketchSet struct { // main struct for storing and querying
		CMS     CMSketch      `json:"cmsketch"`
		HLL     HLLSketch     `json:"hllsketch"`
		DST     DSTSketch     `json:"distribution"`
		TOP     Top16Elements `json:"top16"`
		Minimum int           `json:"minimum"` // Top16 minimum pointer
	}

	DistributionSet struct { // for serving distribution requests
		Cardinality  uint64     `json:"cardinality"`
		Distribution *DSTSketch `json:"distribution"`
		hll          *HLLSketch
	}

	TopSet struct { // for serving top16 requests
		TOP *Top16Elements `json:"top16"`
	}
)

const (
	offset     uint64  = 14695981039346656037 // FNV1A-64 offset
	prime      uint64  = 1099511628211        // FNV1A-64 prime
	width      uint64  = 32768                // For CMS; explicitly specify uint64
	correction float64 = 3023758.39155        // For HLL; 2048*2048*0.72
)

func Hash(data *[]byte) uint64 {
	var hash uint64 = offset
	for _, c := range *data {
		hash ^= uint64(c)
		hash *= prime
	}
	return hash
}

func (c *CMSketch) Add(hash *uint64) {
	for i := 0; i < 4; i++ {
		c[width*uint64(i)+((*hash>>(i*15))&0x7FFF)] += 1
	} // get 4 15-bit hashes from single 64-bit hash
}

func (c *CMSketch) Count(hash *uint64) uint64 {
	var estimate uint64 = 0xFFFFFFFFFFFFFFFF
	for i := 0; i < 4; i++ {
		var index uint64 = width*uint64(i) + ((*hash >> (i * 15)) & 0x7FFF)
		if c[index] < estimate {
			estimate = c[index]
		}
	}
	return estimate
}

func (h *HLLSketch) Add(hash *uint64) {
	var bucket uint64 = *hash >> 53                                        // get first 11 bits to select from 2048 buckets
	var zeroes uint8 = uint8(bits.LeadingZeros64((*hash<<11)|(1<<10)) + 1) // zeroes from remaining bits
	if zeroes > h[bucket] {
		h[bucket] = zeroes
	}
}

func (h *HLLSketch) Count() uint64 {
	var sum float64 = 0
	for _, zeroes := range h {
		sum += 1.0 / float64(uint64(1)<<zeroes)
	}
	// return uint64(correction / sum)
	// directly return and remove the condition check
	// when cardinality estimate reaches > 5200
	var estimate float64 = correction / sum
	if estimate <= 5120 {
		var v uint = 0
		for _, r := range h {
			if r == 0 {
				v++
			}
		}
		if v > 0 {
			estimate = 2048 * math.Log(2048/float64(v))
		}
	}
	return uint64(estimate)
}

func (t *Top16Elements) Update(count *uint64, data *[]byte, minimum *int) {
	if *count < t[*minimum].Count {
		return
	}
	var same bool = false
	var tempChars [128]byte
	copy(tempChars[:], *data)
	for i := 0; i < 16; i++ {
		if t[i].Element == tempChars {
			t[i].Count = *count
			same = true
		}
	}
	if !same {
		t[*minimum].Element = tempChars
		t[*minimum].Count = *count
	}
	for i := 0; i < 16; i++ {
		if t[i].Count < t[*minimum].Count {
			*minimum = i
		}
	} // min pointer must updated only after updating count
}

func (d *DSTSketch) Add(count *uint64, hash *uint64) {
	d[*hash&0xFF] = *count
} // acts as (skewed) random sampling

func (d *DSTSketch) Rank(count *uint64) uint8 {
	var rank uint8 = 0xFF
	for i := 0; i < 256; i++ {
		if d[i] <= *count {
			rank++
		}
	}
	return rank
}

func (s *SketchSet) Add(data *[]byte) uint64 {
	var hash uint64 = Hash(data)
	s.HLL.Add(&hash)
	s.CMS.Add(&hash)
	var count uint64 = s.CMS.Count(&hash)
	s.DST.Add(&count, &hash)
	s.TOP.Update(&count, data, &s.Minimum)
	return count
}

func (s *SketchSet) Read(filename string) {
	file, _ := os.Open(filename)
	gob.NewDecoder(file).Decode(s)
	file.Close()
}

func (s *SketchSet) Write(filename string) {
	file, _ := os.Create(filename)
	gob.NewEncoder(file).Encode(s)
	file.Close()
}

func (d *DistributionSet) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "http://ekunazanu.foo")
	w.Header().Set("Access-Control-Allow-Methods", "GET")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	d.Cardinality = d.hll.Count()
	json.NewEncoder(w).Encode(*d)
}

func (s *TopSet) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "https://ekunazanu.foo")
	w.Header().Set("Access-Control-Allow-Methods", "GET")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	json.NewEncoder(w).Encode(*s)
}

func (s *SketchSet) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Access-Control-Allow-Origin", "https://ekunazanu.foo")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	var data []byte = make([]byte, 128, 128)
	r.Body.Read(data)
	r.Body.Close()
	if r.Method == http.MethodPost {
		fmt.Fprintf(w, "%d", s.Add(&data))
	}
}

func main() {
	var mainDB SketchSet
	var topDB TopSet = TopSet{TOP: &mainDB.TOP}
	var dstDB DistributionSet = DistributionSet{Distribution: &mainDB.DST, hll: &mainDB.HLL}
	mainDB.Read("sketch.gob")    // load previously saved db
	mux := http.NewServeMux()    // create routes for below paths
	mux.Handle("/item", &mainDB) // add element and serve count for /item
	mux.Handle("/dist", &dstDB)  // serve dist for /dist
	mux.Handle("/", &topDB)      // serve top16 for everything else
	go func() {
		for {
			mainDB.Write("sketch.gob")
			time.Sleep(2 * time.Minute)
		}
	}()
	// http.ListenAndServeTLS(":4343", "cert.pem", "private.key.pem", mux)
	http.ListenAndServe(":8080", mux)
}
