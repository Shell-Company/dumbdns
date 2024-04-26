package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Jeffail/gabs/v2"
	"github.com/miekg/dns"
)

var (
	dnsListeningAddress  = flag.String("dns-listening-address", ":53", "DNS listening address")
	llmEndpoint          = flag.String("llm-endpoint", "http://localhost:8080", "TGI LLM endpoint")
	llmMaxNewTokens      = flag.Int("llm-max-new-tokens", 20, "TGI LLM max new tokens")
	llmTemperature       = flag.Float64("llm-temperature", 1.0, "TGI LLM temperature")
	llmTopK              = flag.Int("llm-top-k", 40, "TGI LLM top k")
	llmTopP              = flag.Float64("llm-top-p", 0.2, "TGI LLM top p")
	llmStop              = flag.String("llm-stop", "</s>", "TGI LLM stop token")
	llmSeed              = flag.Int("llm-seed", 0, "TGI LLM seed")
	flagVerbose          = flag.Bool("verbose", false, "Print verbose output")
	flagSystemPromptFile = flag.String("system-prompt", "", "System prompt")
	flagRateLimit        = flag.Int("rate-limit", 0, "Rate limit in requests per second")
	currentRequestTime   time.Time
	previousRequestTime  time.Time
	requests             int
)

func main() {

	startDumbDNS()
}

func RateLimit() bool {
	lastRequestTime := time.Now()
	// check if the time between the last request and the previous request is less than a second
	if lastRequestTime.Sub(previousRequestTime) < time.Second {
		requests++
		if requests > *flagRateLimit {
			time.Sleep(time.Second - lastRequestTime.Sub(previousRequestTime))
		}
	} else {
		requests = 0
	}
	previousRequestTime = currentRequestTime
	// check the rate limit
	if requests > *flagRateLimit {
		return true
	} else {
		return false
	}

}

func startDumbDNS() {
	flag.Parse()

	// handle ALL domains
	dns.HandleFunc(".", handleRequest)
	server := &dns.Server{Addr: *dnsListeningAddress, Net: "udp"}
	serverTCP := &dns.Server{Addr: *dnsListeningAddress, Net: "tcp"}
	server.ListenAndServe()
	serverTCP.ListenAndServe()
}

func handleRequest(w dns.ResponseWriter, r *dns.Msg) {
	// check the rate limit
	if RateLimit() {
		return

	}

	// get the query from the request and initialize the variables
	query := r.Question[0].Name
	tld := ""
	subDomain := ""

	// split the domain name into its components
	domainNameComponents := dns.SplitDomainName(query)

	// if the domain has at minimum a TLD break into subdomain and TLD
	if len(domainNameComponents) > 2 {
		tld = domainNameComponents[len(domainNameComponents)-2] + "." + domainNameComponents[len(domainNameComponents)-1]
		subDomain = ""
		for i := 0; i < len(domainNameComponents)-2; i++ {
			subDomain += domainNameComponents[i]
			if i < len(domainNameComponents)-3 {
				subDomain += "."
			}
		}
	} else {
		// if the domain has no TLD, set the subdomain to the domain and the TLD to an empty string
		// pass the entire query to the TGI endpoint
		subDomain = ""
		tld = ""
	}
	recordType := dns.TypeToString[r.Question[0].Qtype]

	if *flagVerbose {
		println("Query: ", query)
		println("Subdomain: ", subDomain)
		println("TLD: ", tld)
		println("Record Type: ", recordType)
	}

	// create a new DNS message
	m := new(dns.Msg)
	m.SetReply(r)

	// set the response to authoritative - This prevents the client from querying other DNS servers
	m.Authoritative = true

	// construct our prompt by replacing the dots with spaces and adding a sentence end token
	prompt := strings.ReplaceAll(query, ".", " ") + "</s>"

	// for TXT records query the TGI endpoint with the prompt and return the response
	if recordType == "TXT" {

		// initialize system prompt
		systemPrompt := ""
		_ = systemPrompt

		// get system prompt from system_prompt.txt
		if *flagSystemPromptFile != "" {
			systemPromptBytes, err := os.ReadFile(*flagSystemPromptFile)
			if err != nil {
				log.Println("Error reading system prompt: ", err)
			}
			systemPrompt = string(systemPromptBytes)
		}

		//set response seed between 0 and 1000
		seedValue := rand.Intn(1000)
		_ = seedValue
		if *llmSeed != 0 {
			seedValue = *llmSeed
		}

		// query the TGI endpoint
		response, err := queryTGIEndpoint("<|system|>"+systemPrompt+"<|user|>"+prompt, seedValue, *llmMaxNewTokens, float32(*llmTemperature), *llmTopK, float32(*llmTopP), []string{*llmStop})
		if err != nil {
			log.Println("Error querying TGI endpoint: ", err)
		}

		// remove the first two lines of the response - TGI endpoint response format
		responseLines := strings.Split(response, "\n")
		if len(responseLines) > 2 {
			response = strings.Join(responseLines[2:], "\n")
		}
		// clean up the response
		response = strings.ReplaceAll(response, "<|assistant|>", "")

		// split the response into parts if it is longer than 255 characters
		var responseParts []string
		if len(response) > 255 {
			responseParts = []string{}
			for len(response) > 255 {
				responseParts = append(responseParts, response[:255])
				response = response[255:]
			}
			responseParts = append(responseParts, response)
		} else {
			responseParts = []string{response}
		}

		//
		m.Answer = append(m.Answer, &dns.TXT{
			Hdr: dns.RR_Header{Name: query, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 3600},
			Txt: responseParts,
		})

	}
	w.WriteMsg(m)
}

func queryTGIEndpoint(inputs string, seed int, max_new_tokens int, temperature float32, top_k int, top_p float32, stop []string) (response string, err error) {
	// query the TGI endpoint with the input and get the response
	llmRequestBody := map[string]interface{}{
		"inputs": inputs,
		"parameters": map[string]interface{}{
			"max_new_tokens": max_new_tokens,
			"temperature":    temperature,
			"top_k":          top_k,
			"top_p":          top_p,
			"stop":           stop,
			"seed":           seed,
		},
	}
	// marshal Request
	llmRequestBodyJSON, err := json.Marshal(llmRequestBody)
	if err != nil {
		log.Println("Error marshalling request body: ", err)
	}
	// build Request
	llmRequest, err := http.NewRequest("POST", *llmEndpoint+"/generate", bytes.NewBuffer(llmRequestBodyJSON))
	if err != nil {
		log.Println("Error creating request: ", err)
	}
	// set headers
	llmRequest.Header.Set("Content-Type", "application/json")
	llmRequest.Header.Set("User-Agent", "Dumb DNS")

	// send Request
	llmClient := &http.Client{}
	llmResponse, err := llmClient.Do(llmRequest)
	if err != nil {
		log.Println("Error sending request: ", err)
	}
	defer llmResponse.Body.Close()
	// read Response
	llmResponseJSON, err := io.ReadAll(llmResponse.Body)
	if err != nil {
		log.Println("Error reading response: ", err)
	}
	// unmarshal Response
	temporaryContainer, err := gabs.ParseJSON(llmResponseJSON)
	if err != nil {
		log.Println("Error parsing response: ", err, string(llmResponseJSON))
	}
	// get the generated text
	response = temporaryContainer.Path("generated_text").Data().(string)

	// return the generated text
	return response, nil

}
