package main

import (
	"fmt"
	"os"
	"regexp"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: jira-ticket-check <pr_description>")
		os.Exit(1)
	}

	prDescription := os.Args[1]
	fmt.Printf("%s\n", prDescription)
	regexPattern := `\[[A-Z]{2,4}-[1-9]{1,5}\]`

	match, err := regexp.MatchString(regexPattern, prDescription)
	if err != nil {
		fmt.Println("Error:", err)
		os.Exit(1)
	}

	if match {
		fmt.Printf("Jira reference found! PR description matches the pattern (%s)\n", regexPattern)
	} else {
		fmt.Printf("Jira reference not found. PR description does not match the pattern (%s)\n", regexPattern)
		fmt.Println("Check for surrounding '[' and ']'")
		os.Exit(1)
	}
}
