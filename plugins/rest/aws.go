// Copyright 2019 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

package rest

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-ini/ini"
	"github.com/open-policy-agent/opa/internal/providers/aws"
	"github.com/open-policy-agent/opa/logging"
)

const (
	// ref. https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/iam-roles-for-amazon-ec2.html
	ec2DefaultCredServicePath = "http://169.254.169.254/latest/meta-data/iam/security-credentials/"

	// ref. https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/configuring-instance-metadata-service.html
	ec2DefaultTokenPath = "http://169.254.169.254/latest/api/token"

	// ref. https://docs.aws.amazon.com/AmazonECS/latest/userguide/task-iam-roles.html
	ecsDefaultCredServicePath = "http://169.254.170.2"
	ecsRelativePathEnvVar     = "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"

	// ref. https://docs.aws.amazon.com/IAM/latest/UserGuide/id_credentials_temp_enable-regions.html
	stsDefaultPath = "https://sts.amazonaws.com"
	stsRegionPath  = "https://sts.%s.amazonaws.com"

	// ref. https://docs.aws.amazon.com/cli/latest/userguide/cli-configure-envvars.html
	accessKeyEnvVar               = "AWS_ACCESS_KEY_ID"
	secretKeyEnvVar               = "AWS_SECRET_ACCESS_KEY"
	securityTokenEnvVar           = "AWS_SECURITY_TOKEN"
	sessionTokenEnvVar            = "AWS_SESSION_TOKEN"
	awsRegionEnvVar               = "AWS_REGION"
	awsRoleArnEnvVar              = "AWS_ROLE_ARN"
	awsWebIdentityTokenFileEnvVar = "AWS_WEB_IDENTITY_TOKEN_FILE"
	awsCredentialsFileEnvVar      = "AWS_SHARED_CREDENTIALS_FILE"
	awsProfileEnvVar              = "AWS_PROFILE"

	// ref. https://docs.aws.amazon.com/sdkref/latest/guide/settings-global.html
	accessKeyGlobalSetting     = "aws_access_key_id"
	secretKeyGlobalSetting     = "aws_secret_access_key"
	securityTokenGlobalSetting = "aws_session_token"
)

// awsCredentialService represents the interface for AWS credential providers
type awsCredentialService interface {
	credentials() (aws.Credentials, error)
}

// awsEnvironmentCredentialService represents an static environment-variable credential provider for AWS
type awsEnvironmentCredentialService struct {
	logger logging.Logger
}

func (cs *awsEnvironmentCredentialService) credentials() (aws.Credentials, error) {
	var creds aws.Credentials
	creds.AccessKey = os.Getenv(accessKeyEnvVar)
	if creds.AccessKey == "" {
		return creds, errors.New("no " + accessKeyEnvVar + " set in environment")
	}
	creds.SecretKey = os.Getenv(secretKeyEnvVar)
	if creds.SecretKey == "" {
		return creds, errors.New("no " + secretKeyEnvVar + " set in environment")
	}
	creds.RegionName = os.Getenv(awsRegionEnvVar)
	if creds.RegionName == "" {
		return creds, errors.New("no " + awsRegionEnvVar + " set in environment")
	}
	// SessionToken is required if using temporary ENV credentials from assumed IAM role
	// Missing SessionToken results with 403 s3 error.
	creds.SessionToken = os.Getenv(sessionTokenEnvVar)
	if creds.SessionToken == "" {
		// In case of missing SessionToken try to get SecurityToken
		// AWS switched to use SessionToken, but SecurityToken was left for backward compatibility
		creds.SessionToken = os.Getenv(securityTokenEnvVar)
	}

	return creds, nil
}

// awsProfileCredentialService represents a credential provider for AWS that extracts credentials from the AWS
// credentials file
type awsProfileCredentialService struct {

	// Path to the credentials file.
	//
	// If empty will look for "AWS_SHARED_CREDENTIALS_FILE" env variable. If the
	// env value is empty will default to current user's home directory.
	// Linux/OSX: "$HOME/.aws/credentials"
	// Windows:   "%USERPROFILE%\.aws\credentials"
	Path string `json:"path,omitempty"`

	// AWS Profile to extract credentials from the credentials file. If empty
	// will default to environment variable "AWS_PROFILE" or "default" if
	// environment variable is also not set.
	Profile string `json:"profile,omitempty"`

	RegionName string `json:"aws_region"`

	logger logging.Logger
}

func (cs *awsProfileCredentialService) credentials() (aws.Credentials, error) {
	var creds aws.Credentials

	filename, err := cs.path()
	if err != nil {
		return creds, err
	}

	cfg, err := ini.Load(filename)
	if err != nil {
		return creds, fmt.Errorf("failed to read credentials file: %v", err)
	}

	profile, err := cfg.GetSection(cs.profile())
	if err != nil {
		return creds, fmt.Errorf("failed to get profile: %v", err)
	}

	creds.AccessKey = profile.Key(accessKeyGlobalSetting).String()
	if creds.AccessKey == "" {
		return creds, fmt.Errorf("profile \"%v\" in credentials file %v does not contain \"%v\"", cs.Profile, cs.Path, accessKeyGlobalSetting)
	}

	creds.SecretKey = profile.Key(secretKeyGlobalSetting).String()
	if creds.SecretKey == "" {
		return creds, fmt.Errorf("profile \"%v\" in credentials file %v does not contain \"%v\"", cs.Profile, cs.Path, secretKeyGlobalSetting)
	}

	creds.SessionToken = profile.Key(securityTokenGlobalSetting).String() // default to empty string

	if cs.RegionName == "" {
		if cs.RegionName = os.Getenv(awsRegionEnvVar); cs.RegionName == "" {
			return creds, errors.New("no " + awsRegionEnvVar + " set in environment or configuration")
		}
	}
	creds.RegionName = cs.RegionName

	return creds, nil
}

func (cs *awsProfileCredentialService) path() (string, error) {
	if len(cs.Path) != 0 {
		return cs.Path, nil
	}

	if cs.Path = os.Getenv(awsCredentialsFileEnvVar); len(cs.Path) != 0 {
		return cs.Path, nil
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("user home directory not found: %w", err)
	}

	cs.Path = filepath.Join(homeDir, ".aws", "credentials")

	return cs.Path, nil
}

func (cs *awsProfileCredentialService) profile() string {
	if cs.Profile != "" {
		return cs.Profile
	}

	cs.Profile = os.Getenv(awsProfileEnvVar)

	if cs.Profile == "" {
		cs.Profile = "default"
	}

	return cs.Profile
}

// awsMetadataCredentialService represents an EC2 metadata service credential provider for AWS
type awsMetadataCredentialService struct {
	RoleName        string `json:"iam_role,omitempty"`
	RegionName      string `json:"aws_region"`
	creds           aws.Credentials
	expiration      time.Time
	credServicePath string
	tokenPath       string
	logger          logging.Logger
}

func (cs *awsMetadataCredentialService) urlForMetadataService() (string, error) {
	// override default path for testing
	if cs.credServicePath != "" {
		return cs.credServicePath + cs.RoleName, nil
	}
	// otherwise, normal flow
	// if a role name is provided, look up via the EC2 credential service
	if cs.RoleName != "" {
		return ec2DefaultCredServicePath + cs.RoleName, nil
	}
	// otherwise, check environment to see if it looks like we're in an ECS
	// container (with implied role association)
	if isECS() {
		return ecsDefaultCredServicePath + os.Getenv(ecsRelativePathEnvVar), nil
	}
	// if there's no role name and we don't appear to have a path to the
	// ECS container service, then the configuration is invalid
	return "", errors.New("metadata endpoint cannot be determined from settings and environment")
}

func (cs *awsMetadataCredentialService) tokenRequest() (*http.Request, error) {
	tokenURL := ec2DefaultTokenPath
	if cs.tokenPath != "" {
		// override for testing
		tokenURL = cs.tokenPath
	}
	req, err := http.NewRequest(http.MethodPut, tokenURL, nil)
	if err != nil {
		return nil, err
	}

	// we are going to use the token in the immediate future, so a long TTL is not necessary
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "60")
	return req, nil
}

func (cs *awsMetadataCredentialService) refreshFromService() error {
	// define the expected JSON payload from the EC2 credential service
	// ref. https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/iam-roles-for-amazon-ec2.html
	type metadataPayload struct {
		Code            string
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string
		Token           string
		Expiration      time.Time
	}

	// short circuit if a reasonable amount of time until credential expiration remains
	if time.Now().Add(time.Minute * 5).Before(cs.expiration) {
		cs.logger.Debug("Credentials previously obtained from metadata service still valid.")
		return nil
	}

	cs.logger.Debug("Obtaining credentials from metadata service.")
	metaDataURL, err := cs.urlForMetadataService()
	if err != nil {
		// configuration issue or missing ECS environment
		return err
	}

	// construct an HTTP client with a reasonably short timeout
	client := &http.Client{Timeout: time.Second * 10}
	req, err := http.NewRequest(http.MethodGet, metaDataURL, nil)
	if err != nil {
		return errors.New("unable to construct metadata HTTP request: " + err.Error())
	}

	// if in the EC2 environment, we will use IMDSv2, which requires a session cookie from a
	// PUT request on the token endpoint before it will give the credentials, this provides
	// protection from SSRF attacks
	if !isECS() {
		tokenReq, err := cs.tokenRequest()
		if err != nil {
			return errors.New("unable to construct metadata token HTTP request: " + err.Error())
		}
		body, err := doMetaDataRequestWithClient(tokenReq, client, "metadata token", cs.logger)
		if err != nil {
			return err
		}
		// token is the body of response; add to header of metadata request
		req.Header.Set("X-aws-ec2-metadata-token", string(body))
	}

	body, err := doMetaDataRequestWithClient(req, client, "metadata", cs.logger)
	if err != nil {
		return err
	}

	var payload metadataPayload
	err = json.Unmarshal(body, &payload)
	if err != nil {
		return errors.New("failed to parse credential response from metadata service: " + err.Error())
	}

	// Only the EC2 endpoint returns the "Code" element which indicates whether the query was
	// successful; the ECS endpoint does not! Some other fields are missing in the ECS payload
	// but we do not depend on them.
	if cs.RoleName != "" && payload.Code != "Success" {
		return errors.New("metadata service query did not succeed: " + payload.Code)
	}

	cs.expiration = payload.Expiration
	cs.creds.AccessKey = payload.AccessKeyID
	cs.creds.SecretKey = payload.SecretAccessKey
	cs.creds.SessionToken = payload.Token
	cs.creds.RegionName = cs.RegionName

	return nil
}

func (cs *awsMetadataCredentialService) credentials() (aws.Credentials, error) {
	err := cs.refreshFromService()
	if err != nil {
		return cs.creds, err
	}
	return cs.creds, nil
}

// awsWebIdentityCredentialService represents an STS WebIdentity credential services
type awsWebIdentityCredentialService struct {
	RoleArn              string
	WebIdentityTokenFile string
	RegionName           string `json:"aws_region"`
	SessionName          string `json:"session_name"`
	stsURL               string
	creds                aws.Credentials
	expiration           time.Time
	logger               logging.Logger
}

func (cs *awsWebIdentityCredentialService) populateFromEnv() error {
	cs.RoleArn = os.Getenv(awsRoleArnEnvVar)
	if cs.RoleArn == "" {
		return errors.New("no " + awsRoleArnEnvVar + " set in environment")
	}
	cs.WebIdentityTokenFile = os.Getenv(awsWebIdentityTokenFileEnvVar)
	if cs.WebIdentityTokenFile == "" {
		return errors.New("no " + awsWebIdentityTokenFileEnvVar + " set in environment")
	}

	if cs.RegionName == "" {
		if cs.RegionName = os.Getenv(awsRegionEnvVar); cs.RegionName == "" {
			return errors.New("no " + awsRegionEnvVar + " set in environment or configuration")
		}
	}
	return nil
}

func (cs *awsWebIdentityCredentialService) stsPath() string {
	var stsPath string
	switch {
	case cs.stsURL != "":
		stsPath = cs.stsURL
	case cs.RegionName != "":
		stsPath = fmt.Sprintf(stsRegionPath, strings.ToLower(cs.RegionName))
	default:
		stsPath = stsDefaultPath
	}
	return stsPath
}

func (cs *awsWebIdentityCredentialService) refreshFromService() error {
	// define the expected JSON payload from the EC2 credential service
	// ref. https://docs.aws.amazon.com/STS/latest/APIReference/API_AssumeRoleWithWebIdentity.html
	type responsePayload struct {
		Result struct {
			Credentials struct {
				SessionToken    string
				SecretAccessKey string
				Expiration      time.Time
				AccessKeyID     string `xml:"AccessKeyId"`
			}
		} `xml:"AssumeRoleWithWebIdentityResult"`
	}

	// short circuit if a reasonable amount of time until credential expiration remains
	if time.Now().Add(time.Minute * 5).Before(cs.expiration) {
		cs.logger.Debug("Credentials previously obtained from sts service still valid.")
		return nil
	}

	cs.logger.Debug("Obtaining credentials from sts for role %s.", cs.RoleArn)

	var sessionName string
	if cs.SessionName == "" {
		sessionName = "open-policy-agent"
	} else {
		sessionName = cs.SessionName
	}

	tokenData, err := os.ReadFile(cs.WebIdentityTokenFile)
	if err != nil {
		return errors.New("unable to read web token for sts HTTP request: " + err.Error())
	}

	token := string(tokenData)

	queryVals := url.Values{
		"Action":           []string{"AssumeRoleWithWebIdentity"},
		"RoleSessionName":  []string{sessionName},
		"RoleArn":          []string{cs.RoleArn},
		"WebIdentityToken": []string{token},
		"Version":          []string{"2011-06-15"},
	}
	stsRequestURL, _ := url.Parse(cs.stsPath())

	// construct an HTTP client with a reasonably short timeout
	client := &http.Client{Timeout: time.Second * 10}
	req, err := http.NewRequest(http.MethodPost, stsRequestURL.String(), strings.NewReader(queryVals.Encode()))
	if err != nil {
		return errors.New("unable to construct STS HTTP request: " + err.Error())
	}

	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	body, err := doMetaDataRequestWithClient(req, client, "STS", cs.logger)
	if err != nil {
		return err
	}

	var payload responsePayload
	err = xml.Unmarshal(body, &payload)
	if err != nil {
		return errors.New("failed to parse credential response from STS service: " + err.Error())
	}

	cs.expiration = payload.Result.Credentials.Expiration
	cs.creds.AccessKey = payload.Result.Credentials.AccessKeyID
	cs.creds.SecretKey = payload.Result.Credentials.SecretAccessKey
	cs.creds.SessionToken = payload.Result.Credentials.SessionToken
	cs.creds.RegionName = cs.RegionName

	return nil
}

func (cs *awsWebIdentityCredentialService) credentials() (aws.Credentials, error) {
	err := cs.refreshFromService()
	if err != nil {
		return cs.creds, err
	}
	return cs.creds, nil
}

func isECS() bool {
	// the special relative path URI is set by the container agent in the ECS environment only
	_, isECS := os.LookupEnv(ecsRelativePathEnvVar)
	return isECS
}

func doMetaDataRequestWithClient(req *http.Request, client *http.Client, desc string, logger logging.Logger) ([]byte, error) {
	// convenience function to get the body of an AWS EC2 metadata service request with
	// appropriate error-handling boilerplate and logging for this special case
	resp, err := client.Do(req)
	if err != nil {
		// some kind of catastrophe talking to the EC2 service
		return nil, errors.New(desc + " HTTP request failed: " + err.Error())
	}
	defer resp.Body.Close()

	logger.WithFields(map[string]interface{}{
		"url":     req.URL.String(),
		"status":  resp.Status,
		"headers": resp.Header,
	}).Debug("Received response from " + desc + " service.")

	if resp.StatusCode != 200 {
		if logger.GetLevel() == logging.Debug {
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				logger.Debug("Error response with response body: %v", body)
			}
		}
		// could be 404 for role that's not available, but cover all the bases
		return nil, errors.New(desc + " HTTP request returned unexpected status: " + resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		// deal with problems reading the body, whatever those might be
		return nil, errors.New(desc + " HTTP response body could not be read: " + err.Error())
	}
	return body, nil
}

// signV4 modifies an http.Request to include an AWS V4 signature based on a credential provider
func signV4(req *http.Request, service string, credService awsCredentialService, theTime time.Time, sigVersion string) error {
	// General ref. https://docs.aws.amazon.com/general/latest/gr/sigv4_signing.html
	// S3 ref. https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-auth-using-authorization-header.html
	// APIGateway ref. https://docs.aws.amazon.com/apigateway/api-reference/signing-requests/

	var body []byte
	if req.Body == nil {
		body = []byte("")
	} else {
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			return errors.New("error getting request body: " + err.Error())
		}
		// Since ReadAll consumed the body ReadCloser, we must create a new ReadCloser for the request so that the
		// subsequent read starts from the beginning
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	creds, err := credService.credentials()
	if err != nil {
		return errors.New("error getting AWS credentials: " + err.Error())
	}

	now := theTime.UTC()

	if sigVersion == "4a" {
		signedHeaders := aws.SignV4a(req.Header, req.Method, req.URL, body, service, creds, now)
		req.Header = signedHeaders
	} else {
		authHeader, awsHeaders := aws.SignV4(req.Header, req.Method, req.URL, body, service, creds, now)
		req.Header.Set("Authorization", authHeader)
		for k, v := range awsHeaders {
			req.Header.Add(k, v)
		}
	}

	return nil
}
