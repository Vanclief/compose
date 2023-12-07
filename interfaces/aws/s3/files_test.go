package s3

import (
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
)

func (suite *S3Suite) TestListBuckets() {
	list, err := suite.client.ListBuckets()
	suite.Nil(err)
	suite.NotNil(list)
}

func (suite *S3Suite) TestListFiles() {
	input := s3.ListObjectsInput{}

	list, err := suite.client.ListFiles(&input)
	// for _, obj := range objects.Contents {
	// fmt.Println(aws.StringValue(obj.Key))
	// }
	suite.Nil(err)
	suite.NotNil(list)
}

func (suite *S3Suite) TestUploadFile() {
	object := s3.PutObjectInput{
		Key:  aws.String("file.ext"),
		Body: strings.NewReader("The contents of the file."),
		// Metadata: map[string]*string{
		// "x-amz-meta-my-key": aws.String("your-value"),
		// },
	}

	upload, err := suite.client.UploadFile(&object)
	suite.Nil(err)
	suite.NotNil(upload)
}

func (suite *S3Suite) TestDeleteFile() {
	input := &s3.DeleteObjectInput{
		Key: aws.String("file.ext"),
	}

	delete, err := suite.client.DeleteFile(input)
	suite.Nil(err)
	suite.NotNil(delete)
}

func (suite *S3Suite) TestFileExists() {
	// First, upload a file so that we can check if it exists.
	object := &s3.PutObjectInput{
		Key:  aws.String("file_to_test.ext"),
		Body: strings.NewReader("This is a test file."),
	}

	_, err := suite.client.UploadFile(object)
	suite.Nil(err)

	// Now check if the file exists.
	existsInput := &s3.HeadObjectInput{
		Key: aws.String("file_to_test.ext"),
	}

	exists, err := suite.client.FileExists(existsInput)
	suite.Nil(err)
	suite.True(exists)

	// Now delete the file and check if it still exists.
	deleteInput := &s3.DeleteObjectInput{
		Key: aws.String("file_to_test.ext"),
	}

	_, err = suite.client.DeleteFile(deleteInput)
	suite.Nil(err)

	exists, err = suite.client.FileExists(existsInput)
	suite.Nil(err)
	suite.False(exists)
}

func (suite *S3Suite) TestGetPrivateURL() {
	request := &s3.GetObjectInput{
		Key: aws.String("file.ext"),
	}

	url, err := suite.client.GetPrivateURL(request)
	suite.Nil(err)
	suite.NotNil(url)
}
