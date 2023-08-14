package shared

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/breez/lspd/basetypes"
	"github.com/breez/lspd/interceptor"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
)

type OpeningService interface {
	GetFeeParamsMenu(token string, privateKey *btcec.PrivateKey) ([]*OpeningFeeParams, error)
	ValidateOpeningFeeParams(params *OpeningFeeParams, publicKey *btcec.PublicKey) bool
}

type openingService struct {
	store        interceptor.InterceptStore
	nodesService NodesService
}

func NewOpeningService(
	store interceptor.InterceptStore,
	nodesService NodesService,
) OpeningService {
	return &openingService{
		store:        store,
		nodesService: nodesService,
	}
}

type OpeningFeeParams struct {
	MinFeeMsat           uint64 `json:"min_fee_msat,string"`
	Proportional         uint32 `json:"proportional"`
	ValidUntil           string `json:"valid_until"`
	MinLifetime          uint32 `json:"min_lifetime"`
	MaxClientToSelfDelay uint32 `json:"max_client_to_self_delay"`
	Promise              string `json:"promise"`
}

func (s *openingService) GetFeeParamsMenu(token string, privateKey *btcec.PrivateKey) ([]*OpeningFeeParams, error) {
	var menu []*OpeningFeeParams
	settings, err := s.store.GetFeeParamsSettings(token)
	if err != nil {
		log.Printf("Failed to fetch fee params settings: %v", err)
		return nil, fmt.Errorf("failed to get opening_fee_params")
	}

	if len(settings) == 0 {
		log.Printf("No fee params setings found in the db [token=%v]", token)
	}

	for _, setting := range settings {
		validUntil := time.Now().UTC().Add(setting.Validity)
		params := &OpeningFeeParams{
			MinFeeMsat:           setting.Params.MinMsat,
			Proportional:         setting.Params.Proportional,
			ValidUntil:           validUntil.Format(basetypes.TIME_FORMAT),
			MinLifetime:          setting.Params.MaxIdleTime,
			MaxClientToSelfDelay: setting.Params.MaxClientToSelfDelay,
		}

		promise, err := createPromise(privateKey, params)
		if err != nil {
			log.Printf("Failed to create promise: %v", err)
			return nil, err
		}

		params.Promise = *promise
		menu = append(menu, params)
	}

	sort.Slice(menu, func(i, j int) bool {
		if menu[i].MinFeeMsat == menu[j].MinFeeMsat {
			return menu[i].Proportional < menu[j].Proportional
		}

		return menu[i].MinFeeMsat < menu[j].MinFeeMsat
	})
	return menu, nil
}

func (s *openingService) ValidateOpeningFeeParams(params *OpeningFeeParams, publicKey *btcec.PublicKey) bool {
	if params == nil {
		return false
	}

	err := verifyPromise(publicKey, params)
	if err != nil {
		return false
	}

	t, err := time.Parse(basetypes.TIME_FORMAT, params.ValidUntil)
	if err != nil {
		log.Printf("validateOpeningFeeParams: time.Parse(%v, %v) error: %v", basetypes.TIME_FORMAT, params.ValidUntil, err)
		return false
	}

	if time.Now().UTC().After(t) {
		log.Printf("validateOpeningFeeParams: promise not valid anymore: %v", t)
		return false
	}

	return true
}

func createPromise(lspPrivateKey *btcec.PrivateKey, params *OpeningFeeParams) (*string, error) {
	hash, err := paramsHash(params)
	if err != nil {
		return nil, err
	}
	// Sign the hash with the private key of the LSP id.
	sig, err := ecdsa.SignCompact(lspPrivateKey, hash[:], true)
	if err != nil {
		log.Printf("createPromise: SignCompact error: %v", err)
		return nil, err
	}
	promise := hex.EncodeToString(sig)
	return &promise, nil
}

func paramsHash(params *OpeningFeeParams) ([]byte, error) {
	// First hash all the values in the params in a fixed order.
	items := []interface{}{
		params.MinFeeMsat,
		params.Proportional,
		params.ValidUntil,
		params.MinLifetime,
		params.MaxClientToSelfDelay,
	}
	blob, err := json.Marshal(items)
	if err != nil {
		log.Printf("paramsHash error: %v", err)
		return nil, err
	}
	hash := sha256.Sum256(blob)
	return hash[:], nil
}

func verifyPromise(lspPublicKey *btcec.PublicKey, params *OpeningFeeParams) error {
	hash, err := paramsHash(params)
	if err != nil {
		return err
	}
	sig, err := hex.DecodeString(params.Promise)
	if err != nil {
		log.Printf("verifyPromise: hex.DecodeString error: %v", err)
		return err
	}
	pub, _, err := ecdsa.RecoverCompact(sig, hash)
	if err != nil {
		log.Printf("verifyPromise: RecoverCompact(%x) error: %v", sig, err)
		return err
	}
	if !lspPublicKey.IsEqual(pub) {
		log.Print("verifyPromise: not signed by us", err)
		return fmt.Errorf("invalid promise")
	}
	return nil
}
