package autoquery

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbiface"
)

// Client is a querying client for DynamoDB that enables automatic index selection.
// The client caches table metadata to optimize calls on previously-queried tables.
type Client struct {
	dynamodbService dynamodbiface.DynamoDBAPI

	metadataProvider TableDescriptionProvider

	tableIndexMetadataCache map[string]*tableIndexMetadata

	// SecondaryIndexSparsenessThreshold sets the threshold for secondary indexes to be considered
	// sparse vs non-sparse. This does not apply to the primary table index, which is never
	// sparse.
	//
	// When a table's metadata is gathered, if the ratio of number of items in the secondary index
	// to number of items in the table is greater than or equal to
	// SecondaryIndexSparsenessThreshold, then the index will be considered non-sparse for
	// purposes of index selection. Sparse indexes are only viable for query expressions which
	// filter on both the primary and sort keys of the index, in order to ensure that all items
	// matched on the query are returned.
	//
	// If the SecondaryIndexSparsenessThreshold is set to a value less than or equal to 0.0, then
	// all secondary indexes will be considered non-sparse; if set to 1.0, then each secondary
	// index will be considered non-sparse if the number of items in the index matches the total
	// number of items in the table. If set to a value greater than 1.0, then all secondary
	// indexes will be considered sparse.
	//
	// By default, all secondary indexes are considered sparse. If non-default behavior is
	// desired, this value should be set before any queries are parsed with Parser.Next.
	SecondaryIndexSparsenessThreshold float64
}

// NewClient creates a new Client instance.
func NewClient(service dynamodbiface.DynamoDBAPI) *Client {
	return NewClientWithMetadataProvider(service, newDefaultDescriptionProvider(service))
}

// NewClientWithMetadataProvider creates a new Client instance with a specified metadata provider.
//
// Specifying alternate metadata providers is an advanced feature.
// Most users should use NewClient when creating an auto-querying client, which uses DescribeTable
// as the metadata provider.
//
// An alternative TableDescriptionProvider may be needed in cases where the table cannot be
// described using DescribeTable.
func NewClientWithMetadataProvider(
	service dynamodbiface.DynamoDBAPI, provider TableDescriptionProvider) *Client {
	return &Client{
		dynamodbService:         service,
		metadataProvider:        provider,
		tableIndexMetadataCache: map[string]*tableIndexMetadata{},
		// by default, all secondary indexes are considered sparse
		SecondaryIndexSparsenessThreshold: 1.1,
	}
}

// NewQuery initializes a query defined by expr on a table. The returned parser may be used to
// retrieve items using Parser.Next.
//
// On the first call to a new table, the client will populate the table's index metadata using
// the underlying metadata provider. The metadata is cached for subsequent queries to the table
// through the same Client instance. The query automatically selects an index based on the table
// metadata and any expression restrictions.
func (client *Client) NewQuery(tableName string, expr *Expression) *Parser {
	return &Parser{
		client:        client,
		tableName:     tableName,
		expr:          expr,
		bufferedItems: []map[string]*dynamodb.AttributeValue{},
	}
}

func (client *Client) pullIndexMetadata(
	ctx context.Context, tableName string) (*tableIndexMetadata, error) {

	indexMetadata, found := client.tableIndexMetadataCache[tableName]
	if !found {
		// attempt to pull table description from metadata provider
		tableDescription, err := client.metadataProvider.Get(ctx, tableName)
		if err != nil {
			return nil, err
		}
		indexMetadata = client.parseTableIndexMetadata(tableDescription)
		// add metadata to cache
		client.tableIndexMetadataCache[tableName] = indexMetadata
	}

	return indexMetadata, nil
}

func (client *Client) parseTableIndexMetadata(table *dynamodb.TableDescription) *tableIndexMetadata {
	output := &tableIndexMetadata{
		Indexes: []*tableIndex{},
	}

	appendIndex := func(index *tableIndex) {
		output.Indexes = append(output.Indexes, index)
	}

	// extract primary key index
	tableSize := int(*table.ItemCount)
	tablePrimaryIndex := &tableIndex{
		Name:                  tablePrimaryIndexName,
		Size:                  tableSize,
		IncludesAllAttributes: true,
		ConsistentReadable:    true,
		IsSparse:              false,
	}
	tablePrimaryIndex.loadKeysFromSchema(table.KeySchema)
	appendIndex(tablePrimaryIndex)

	tablePrimaryIndexKeys := tablePrimaryIndex.getKeys()

	// extract global secondary indexes
	if table.GlobalSecondaryIndexes != nil {
		for _, gsi := range table.GlobalSecondaryIndexes {
			index := &tableIndex{
				Name: *gsi.IndexName,
				Size: int(*gsi.ItemCount),
				// global secondary indexes do not support consistent read
				ConsistentReadable: false,
			}
			index.loadKeysFromSchema(gsi.KeySchema)
			index.loadAttributesFromProjection(gsi.Projection, tablePrimaryIndexKeys)
			index.inferSparseness(tableSize, client.SecondaryIndexSparsenessThreshold)
			appendIndex(index)
		}
	}

	// extract local secondary indexes
	if table.LocalSecondaryIndexes != nil {
		for _, lsi := range table.LocalSecondaryIndexes {
			index := &tableIndex{
				Name:               *lsi.IndexName,
				Size:               int(*lsi.ItemCount),
				ConsistentReadable: true,
				IsSparse:           true,
			}
			index.loadKeysFromSchema(lsi.KeySchema)
			index.loadAttributesFromProjection(lsi.Projection, tablePrimaryIndexKeys)
			index.inferSparseness(tableSize, client.SecondaryIndexSparsenessThreshold)
			appendIndex(index)
		}
	}

	return output
}

func (client *Client) chooseIndex(ctx context.Context,
	tableName string, expr *Expression) (*tableIndex, error) {

	// pull metadata from cache
	indexMetadata, err := client.pullIndexMetadata(ctx, tableName)
	if err != nil {
		return nil, err
	}

	var bestIndex *tableIndex
	bestIndexScore := 0.0

	// select index with best score based on the expression
	inviableErrs := []*ErrIndexNotViable{}
	for _, index := range indexMetadata.Indexes {
		indexScore, inviableErr := client.scoreIndexOnExpr(index, expr)
		if inviableErr != nil {
			inviableErrs = append(inviableErrs, inviableErr)
		} else if indexScore > bestIndexScore {
			bestIndex = index
			bestIndexScore = indexScore
		}
	}

	// no viable indexes found
	if bestIndex == nil {
		return nil, &ErrNoViableIndexes{IndexErrs: inviableErrs}
	}

	return bestIndex, nil
}

func (client *Client) constructQueryInputGivenIndex(
	queryIndex *tableIndex) (*dynamodb.QueryInput, error) {

	// TODO: implement
	return nil, fmt.Errorf("not yet implemented")
}

func (client *Client) scoreIndexOnExpr(
	index *tableIndex, expr *Expression) (float64, *ErrIndexNotViable) {

	indexNotViableReasons := client.listIndexViabilityInfractions(index, expr)
	if len(indexNotViableReasons) > 0 {
		return 0.0, &ErrIndexNotViable{
			IndexName:        index.Name,
			NotViableReasons: indexNotViableReasons,
		}
	}

	return 0.0, &ErrIndexNotViable{
		IndexName: index.Name,
		NotViableReasons: []string{
			"not yet implemented",
		},
	}
}

func (client *Client) listIndexViabilityInfractions(
	index *tableIndex, expr *Expression) []string {

	notViableReasons := []string{}

	// for index to be viable, there must be an equals filter on the index's partition key
	if !typesMatch(expr.filters[index.PartitionKey], &equalsFilter{}) {
		reason := fmt.Sprintf(
			"expression does not contain an equals condition on attribute: %s",
			index.PartitionKey)
		notViableReasons = append(notViableReasons, reason)
	}

	// if consistent read is specified, index must be consistent-readable
	if expr.consistentRead && !index.ConsistentReadable {
		notViableReasons = append(notViableReasons,
			"global secondary index does not support consistent read")
	}

	// if order is specified, index must sort on that attribute
	if expr.orderSpecified && expr.orderAttribute != index.SortKey {
		reason := fmt.Sprintf(
			"expression specifies order, which requires an index with sort key: %s",
			index.SortKey)
		notViableReasons = append(notViableReasons, reason)
	}

	// index must include selected attributes, or project all attributes if not specified
	if expr.attributesSpecified {
		indexMissingAttrs := []string{}
		for _, selectedAttr := range expr.attributes {
			if _, found := index.AttributeSet[selectedAttr]; !found {
				indexMissingAttrs = append(indexMissingAttrs, selectedAttr)
			}
		}
		if len(indexMissingAttrs) > 0 {
			reason := fmt.Sprintf("index does not include attributes: %s",
				strings.Join(indexMissingAttrs, ", "))
			notViableReasons = append(notViableReasons, reason)
		}
	} else if !index.IncludesAllAttributes {
		notViableReasons = append(notViableReasons,
			"expression does not select attributes, so it requires an index that projects all")
	}

	// if index is sparse, then both partition and sort attributes must appear in expression
	if index.IsSparse {
		// equals condition on partition key takes precedence, so only need to check sort key
		if _, found := expr.filters[index.SortKey]; !found {
			reason := fmt.Sprintf(
				"expression does not filter on sparse secondary index's sort key: %s",
				index.SortKey)
			notViableReasons = append(notViableReasons, reason)
		}
	}

	return notViableReasons
}
