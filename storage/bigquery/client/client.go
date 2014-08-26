// Copyright 2014 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package client

import (
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"

	"code.google.com/p/goauth2/oauth"
	"code.google.com/p/goauth2/oauth/jwt"
	bigquery "code.google.com/p/google-api-go-client/bigquery/v2"
)

var (
	// TODO(jnagal): Condense all flags to an identity file and a pem key file.
	clientId       = flag.String("bq_id", "", "Client ID")
	clientSecret   = flag.String("bq_secret", "notasecret", "Client Secret")
	projectId      = flag.String("bq_project_id", "", "Bigquery project ID")
	serviceAccount = flag.String("bq_account", "", "Service account email")
	pemFile        = flag.String("bq_credentials_file", "", "Credential Key file (pem)")
)

const (
	errAlreadyExists string = "Error 409: Already Exists"
	queryLimit       int64  = 200
)

type Client struct {
	service   *bigquery.Service
	token     *oauth.Token
	datasetId string
	tableId   string
}

// Helper method to create an authenticated connection.
func connect() (*oauth.Token, *bigquery.Service, error) {
	if *clientId == "" {
		return nil, nil, fmt.Errorf("No client id specified")
	}
	if *serviceAccount == "" {
		return nil, nil, fmt.Errorf("No service account specified")
	}
	if *projectId == "" {
		return nil, nil, fmt.Errorf("No project id specified")
	}
	authScope := bigquery.BigqueryScope
	if *pemFile == "" {
		return nil, nil, fmt.Errorf("No credentials specified")
	}
	pemBytes, err := ioutil.ReadFile(*pemFile)
	if err != nil {
		return nil, nil, fmt.Errorf("Could not access credential file %v - %v", pemFile, err)
	}

	t := jwt.NewToken(*serviceAccount, authScope, pemBytes)
	token, err := t.Assert(&http.Client{})
	if err != nil {
		fmt.Printf("Invalid token: %v\n", err)
		return nil, nil, err
	}
	config := &oauth.Config{
		ClientId:     *clientId,
		ClientSecret: *clientSecret,
		Scope:        authScope,
		AuthURL:      "https://accounts.google.com/o/oauth2/auth",
		TokenURL:     "https://accounts.google.com/o/oauth2/token",
	}

	transport := &oauth.Transport{
		Token:  token,
		Config: config,
	}
	client := transport.Client()

	service, err := bigquery.New(client)
	if err != nil {
		fmt.Printf("Failed to create new service: %v\n", err)
		return nil, nil, err
	}

	return token, service, nil
}

// Creates a new client instance with an authenticated connection to bigquery.
func NewClient() (*Client, error) {
	token, service, err := connect()
	if err != nil {
		return nil, err
	}
	c := &Client{
		token:   token,
		service: service,
	}
	return c, nil
}

func (c *Client) Close() error {
	c.service = nil
	return nil
}

// Helper method to return the bigquery service connection.
// Expired connection is refreshed.
func (c *Client) getService() (*bigquery.Service, error) {
	if c.token == nil || c.service == nil {
		return nil, fmt.Errorf("Service not initialized")
	}

	// Refresh expired token.
	if c.token.Expired() {
		token, service, err := connect()
		if err != nil {
			return nil, err
		}
		c.token = token
		c.service = service
		return service, nil
	}
	return c.service, nil
}

func (c *Client) PrintDatasets() error {
	datasetList, err := c.service.Datasets.List(*projectId).Do()
	if err != nil {
		fmt.Printf("Failed to get list of datasets\n")
		return err
	} else {
		fmt.Printf("Successfully retrieved datasets. Retrieved: %d\n", len(datasetList.Datasets))
	}

	for _, d := range datasetList.Datasets {
		fmt.Printf("%s %s\n", d.Id, d.FriendlyName)
	}
	return nil
}

func (c *Client) CreateDataset(datasetId string) error {
	if c.service == nil {
		return fmt.Errorf("No service created")
	}
	_, err := c.service.Datasets.Insert(*projectId, &bigquery.Dataset{
		DatasetReference: &bigquery.DatasetReference{
			DatasetId: datasetId,
			ProjectId: *projectId,
		},
	}).Do()
	// TODO(jnagal): Do a Get() to verify dataset already exists.
	if err != nil && !strings.Contains(err.Error(), errAlreadyExists) {
		return err
	}
	c.datasetId = datasetId
	return nil
}

// Create a table with provided table ID and schema.
// Schema is currently not updated if the table already exists.
func (c *Client) CreateTable(tableId string, schema *bigquery.TableSchema) error {
	if c.service == nil || c.datasetId == "" {
		return fmt.Errorf("No dataset created")
	}
	_, err := c.service.Tables.Get(*projectId, c.datasetId, tableId).Do()
	if err != nil {
		// Create a new table.
		_, err := c.service.Tables.Insert(*projectId, c.datasetId, &bigquery.Table{
			Schema: schema,
			TableReference: &bigquery.TableReference{
				DatasetId: c.datasetId,
				ProjectId: *projectId,
				TableId:   tableId,
			},
		}).Do()
		if err != nil {
			return err
		}
	}
	// TODO(jnagal): Update schema if it has changed. We can only extend existing schema.
	c.tableId = tableId
	return nil
}

// Add a row to the connected table.
func (c *Client) InsertRow(rowData map[string]interface{}) error {
	service, _ := c.getService()
	if service == nil || c.datasetId == "" || c.tableId == "" {
		return fmt.Errorf("Table not setup to add rows")
	}
	jsonRows := make(map[string]bigquery.JsonValue)
	for key, value := range rowData {
		jsonRows[key] = bigquery.JsonValue(value)
	}
	rows := []*bigquery.TableDataInsertAllRequestRows{
		{
			Json: jsonRows,
		},
	}

	// TODO(jnagal): Batch insert requests.
	insertRequest := &bigquery.TableDataInsertAllRequest{Rows: rows}

	result, err := service.Tabledata.InsertAll(*projectId, c.datasetId, c.tableId, insertRequest).Do()
	if err != nil {
		return fmt.Errorf("Error inserting row: %v", err)
	}

	if len(result.InsertErrors) > 0 {
		return fmt.Errorf("Insertion for %d rows failed")
	}
	return nil
}

// Returns a bigtable table name (format: datasetID.tableID)
func (c *Client) GetTableName() (string, error) {
	if c.service == nil || c.datasetId == "" || c.tableId == "" {
		return "", fmt.Errorf("Table not setup")
	}
	return fmt.Sprintf("%s.%s", c.datasetId, c.tableId), nil
}

// Do a synchronous query on bigtable and return a header and data rows.
// Number of rows are capped to queryLimit.
func (c *Client) Query(query string) ([]string, [][]interface{}, error) {
	service, err := c.getService()
	if err != nil {
		return nil, nil, err
	}
	datasetRef := &bigquery.DatasetReference{
		DatasetId: c.datasetId,
		ProjectId: *projectId,
	}

	queryRequest := &bigquery.QueryRequest{
		DefaultDataset: datasetRef,
		MaxResults:     queryLimit,
		Kind:           "json",
		Query:          query,
	}

	results, err := service.Jobs.Query(*projectId, queryRequest).Do()
	if err != nil {
		return nil, nil, err
	}
	numRows := results.TotalRows
	if numRows < 1 {
		return nil, nil, fmt.Errorf("Query returned no data")
	}

	headers := []string{}
	for _, col := range results.Schema.Fields {
		headers = append(headers, col.Name)
	}

	rows := [][]interface{}{}
	numColumns := len(results.Schema.Fields)
	for _, data := range results.Rows {
		row := make([]interface{}, numColumns)
		for c := 0; c < numColumns; c++ {
			row[c] = data.F[c].V
		}
		rows = append(rows, row)
	}
	return headers, rows, nil
}