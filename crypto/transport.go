package crypto

import (
	"bytes"
	"compress/zlib"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"github.com/golang/protobuf/proto"
	metrics "github.com/rcrowley/go-metrics"
	"io/ioutil"
	"strings"
	"www.velocidex.com/golang/velociraptor/context"
	crypto_proto "www.velocidex.com/golang/velociraptor/crypto/proto"
	"www.velocidex.com/golang/velociraptor/third_party/cache"
)

type _Cipher struct {
	key_size                  int
	cipher_properties         *crypto_proto.CipherProperties
	cipher_metadata           *crypto_proto.CipherMetadata
	encrypted_cipher          []byte
	encrypted_cipher_metadata []byte
}

func (self *_Cipher) Size() int {
	return 1
}

func _NewCipher(
	source string,
	private_key *rsa.PrivateKey,
	certificate *x509.Certificate) (*_Cipher, error) {

	if certificate.PublicKeyAlgorithm != x509.RSA {
		return nil, errors.New("Not RSA algorithm")
	}

	result := &_Cipher{
		key_size: 128,
	}
	cipher_name := "aes_128_cbc"
	result.cipher_properties = &crypto_proto.CipherProperties{
		Name:       &cipher_name,
		Key:        make([]byte, result.key_size/8),
		MetadataIv: make([]byte, result.key_size/8),
		HmacKey:    make([]byte, result.key_size/8),
		HmacType:   crypto_proto.CipherProperties_FULL_HMAC.Enum(),
	}

	_, err := rand.Read(result.cipher_properties.Key)
	if err != nil {
		return nil, err
	}

	_, err = rand.Read(result.cipher_properties.MetadataIv)
	if err != nil {
		return nil, err
	}

	_, err = rand.Read(result.cipher_properties.HmacKey)
	if err != nil {
		return nil, err
	}

	result.cipher_metadata = &crypto_proto.CipherMetadata{
		Source: &source,
	}

	serialized_cipher, err := proto.Marshal(result.cipher_properties)
	if err != nil {
		return nil, err
	}

	hashed := sha256.Sum256(serialized_cipher)
	metrics.GetOrRegisterCounter("rsa.sign", nil).Inc(1)
	signature, err := rsa.SignPKCS1v15(
		rand.Reader, private_key, crypto.SHA256, hashed[:])
	if err != nil {
		return nil, err
	}
	result.cipher_metadata.Signature = signature

	metrics.GetOrRegisterCounter("rsa.encrypt", nil).Inc(1)
	encrypted_cipher, err := rsa.EncryptOAEP(
		sha1.New(), rand.Reader,
		certificate.PublicKey.(*rsa.PublicKey),
		serialized_cipher, []byte(""))
	if err != nil {
		return nil, err
	}

	result.encrypted_cipher = encrypted_cipher

	serialized_cipher_metadata, err := proto.Marshal(result.cipher_metadata)
	if err != nil {
		return nil, err
	}

	encrypted_cipher_metadata, err := encryptSymmetric(
		result.cipher_properties,
		serialized_cipher_metadata,
		result.cipher_properties.MetadataIv)
	if err != nil {
		return nil, err
	}
	result.encrypted_cipher_metadata = encrypted_cipher_metadata

	return result, nil
}

type CryptoManager struct {
	context      *context.Context
	private_key  *rsa.PrivateKey
	certificates map[string]*x509.Certificate

	source string

	// Cache output cipher sessions for each destination. Sending
	// to the same destination will reuse the same cipher object
	// and therefore the same RSA keys.
	output_cipher_cache *cache.LRUCache

	// Cache cipher objects which have been verified.
	input_cipher_cache *cache.LRUCache
}

func (self *CryptoManager) AddCertificate(certificate_pem []byte) error {
	client_cert, err := parseX509CertFromPemStr(certificate_pem)
	if err != nil {
		return err
	}
	self.certificates[client_cert.Subject.CommonName] = client_cert

	return nil
}

func NewCryptoManager(context *context.Context, source string, pem_str []byte) (
	*CryptoManager, error) {
	private_key, err := parseRsaPrivateKeyFromPemStr(pem_str)
	if err != nil {
		return nil, err
	}

	return &CryptoManager{context: context,
		private_key:         private_key,
		source:              source,
		certificates:        make(map[string]*x509.Certificate),
		output_cipher_cache: cache.NewLRUCache(1000),
		input_cipher_cache:  cache.NewLRUCache(1000),
	}, nil
}

// Once a message is decoded the MessageInfo contains metadata about it.
type MessageInfo struct {
	Raw           []byte
	Authenticated bool
	Source        *string
}

/*

  This method can be overridden by derived classes to provide a way of
  recovering the X509 certificate of each source. We use this
  certificate to:

  1. Verify the message signature when receiving a message from a
     particular source.

  2. Encrypt the message using the certificate public key when
     encrypting a message destined to a particular entity.

  Implementations are expected to provide a mapping between known
  sources and their certificates.

*/
func (self *CryptoManager) GetCertificate(subject string) (*x509.Certificate, bool) {
	// GRR sometimes prefixes common names with aff4:/ so strip it first.
	normalized_subject := strings.TrimPrefix(subject, "aff4:/")
	result, pres := self.certificates[normalized_subject]
	return result, pres
}

/* Verify the HMAC protecting the cipher properties blob.

   The HMAC ensures that the cipher properties can not be modified.
*/
func (self *CryptoManager) calcHMAC(
	comms *crypto_proto.ClientCommunication,
	cipher *crypto_proto.CipherProperties) []byte {
	msg := comms.Encrypted
	msg = append(msg, comms.EncryptedCipher...)
	msg = append(msg, comms.EncryptedCipherMetadata...)
	msg = append(msg, comms.PacketIv...)

	temp := make([]byte, 4)
	binary.LittleEndian.PutUint32(temp, *comms.ApiVersion)
	msg = append(msg, temp...)

	mac := hmac.New(sha1.New, cipher.HmacKey)
	mac.Write(msg)

	return mac.Sum(nil)
}

func encryptSymmetric(
	cipher_properties *crypto_proto.CipherProperties,
	plain_text []byte,
	iv []byte) ([]byte, error) {
	if len(cipher_properties.Key) != 16 {
		return nil, errors.New("Incorrect key length provided.")
	}

	// Add padding. See
	// https://tools.ietf.org/html/rfc5246#section-6.2.3.2 for
	// details.
	padding := aes.BlockSize - (len(plain_text) % aes.BlockSize)
	for i := 0; i < padding; i++ {
		plain_text = append(plain_text, byte(padding))
	}

	base_crypter, err := aes.NewCipher(cipher_properties.Key)
	if err != nil {
		return nil, err
	}

	mode := cipher.NewCBCEncrypter(base_crypter, iv)
	cipher_text := make([]byte, len(plain_text))
	mode.CryptBlocks(cipher_text, plain_text)

	return cipher_text, nil
}

func decryptSymmetric(
	cipher_properties *crypto_proto.CipherProperties,
	cipher_text []byte,
	iv []byte) ([]byte, error) {
	if len(cipher_properties.Key) != 16 {
		return nil, errors.New("Incorrect key length provided.")
	}

	if len(cipher_text)%aes.BlockSize != 0 {
		return nil, errors.New("Cipher test is not whole number of blocks")
	}

	base_crypter, err := aes.NewCipher(cipher_properties.Key)
	if err != nil {
		return nil, err
	}

	mode := cipher.NewCBCDecrypter(base_crypter, iv)
	plain_text := make([]byte, len(cipher_text))
	mode.CryptBlocks(plain_text, cipher_text)

	// Strip the padding. See
	// https://tools.ietf.org/html/rfc5246#section-6.2.3.2 for
	// details.
	padding := int(plain_text[len(plain_text)-1])
	for i := len(plain_text) - padding; i < len(plain_text); i++ {
		if int(plain_text[i]) != padding {
			return nil, errors.New("Padding error")
		}
	}

	return plain_text[:len(plain_text)-padding], nil
}

func (self *CryptoManager) getAuthState(
	cipher_metadata *crypto_proto.CipherMetadata,
	serialized_cipher []byte,
	cipher_properties *crypto_proto.CipherProperties) (bool, error) {

	// Verify the cipher signature using the certificate known for
	// the sender.
	cert, pres := self.GetCertificate(*cipher_metadata.Source)
	if !pres {
		// We dont know who we are talking to so we can not
		// trust them.
		return false, errors.New("No cert found")
	}

	hashed := sha256.Sum256(serialized_cipher)
	if cert.PublicKeyAlgorithm != x509.RSA {
		return false, errors.New("Not RSA algorithm")
	}

	public_key := cert.PublicKey.(*rsa.PublicKey)
	metrics.GetOrRegisterCounter("rsa.verify", nil).Inc(1)
	err := rsa.VerifyPKCS1v15(public_key, crypto.SHA256, hashed[:],
		cipher_metadata.Signature)
	if err != nil {
		return false, err
	}

	return true, nil
}

/* Decrypts an encrypted parcel. */
func (self *CryptoManager) Decrypt(cipher_text []byte) (*MessageInfo, error) {
	var err error
	// Parse the ClientCommunication protobuf.
	communications := &crypto_proto.ClientCommunication{}
	err = proto.Unmarshal(cipher_text, communications)
	if err != nil {
		return nil, err
	}

	auth_state := false
	var cipher_properties *crypto_proto.CipherProperties

	v, ok := self.input_cipher_cache.Get(string(communications.EncryptedCipher))
	if ok {
		auth_state = true
		cipher_properties = v.(*_Cipher).cipher_properties

		// Check HMAC to save checking the RSA signature for
		// malformed packets.
		if !hmac.Equal(
			self.calcHMAC(communications, cipher_properties),
			communications.FullHmac) {
			return nil, errors.New("HMAC did not verify")
		}

	} else {
		// Decrypt the CipherProperties
		metrics.GetOrRegisterCounter("rsa.decrypt", nil).Inc(1)
		serialized_cipher, err := rsa.DecryptOAEP(
			sha1.New(), rand.Reader,
			self.private_key,
			communications.EncryptedCipher,
			[]byte(""))
		if err != nil {
			return nil, err
		}

		cipher_properties = &crypto_proto.CipherProperties{}
		err = proto.Unmarshal(serialized_cipher, cipher_properties)
		if err != nil {
			return nil, err
		}

		// Check HMAC first to save checking the RSA signature for
		// malformed packets.
		if !hmac.Equal(
			self.calcHMAC(communications, cipher_properties),
			communications.FullHmac) {
			return nil, errors.New("HMAC did not verify")
		}

		// Extract the serialized CipherMetadata.
		serialized_metadata, err := decryptSymmetric(
			cipher_properties, communications.EncryptedCipherMetadata,
			cipher_properties.MetadataIv)
		if err != nil {
			return nil, err
		}

		cipher_metadata := &crypto_proto.CipherMetadata{}
		err = proto.Unmarshal(serialized_metadata, cipher_metadata)
		if err != nil {
			return nil, err
		}

		// Verify the cipher metadata signature.
		auth_state, err = self.getAuthState(cipher_metadata,
			serialized_cipher,
			cipher_properties)
		if err != nil {
			return nil, err
		}

		if auth_state {
			self.input_cipher_cache.Set(
				string(communications.EncryptedCipher),
				&_Cipher{cipher_properties: cipher_properties},
			)
		}

	}

	// Decrypt the cipher metadata.
	plain, err := decryptSymmetric(
		cipher_properties,
		communications.Encrypted,
		communications.PacketIv)
	if err != nil {
		return nil, err
	}

	// Unpack the message list.
	packed_message_list := &crypto_proto.PackedMessageList{}
	err = proto.Unmarshal(plain, packed_message_list)
	if err != nil {
		return nil, err
	}

	serialized_message_list := packed_message_list.MessageList
	if *packed_message_list.Compression == crypto_proto.PackedMessageList_ZCOMPRESSION {
		b := bytes.NewReader(serialized_message_list)
		z, err := zlib.NewReader(b)
		if err != nil {
			return nil, err
		}
		defer z.Close()
		p, err := ioutil.ReadAll(z)
		if err != nil {
			return nil, err
		}

		serialized_message_list = p
	}

	return &MessageInfo{
		Raw:           serialized_message_list,
		Authenticated: auth_state,
		Source:        packed_message_list.Source,
	}, nil
}

// GRR usually encodes a MessageList protobuf inside the encrypted
// payload. This convenience method parses that type of payload after
// decrypting it. Velociraptor sometimes encrypts other types of
// messages as the payload.
func (self *CryptoManager) DecryptMessageList(cipher_text []byte) (*crypto_proto.MessageList, error) {
	message_info, err := self.Decrypt(cipher_text)
	if err != nil {
		return nil, err
	}

	result := &crypto_proto.MessageList{}
	err = proto.Unmarshal(message_info.Raw, result)
	if err != nil {
		return nil, err
	}

	for _, message := range result.Job {
		if message_info.Authenticated {
			auth := crypto_proto.GrrMessage_AUTHENTICATED
			message.AuthState = &auth
		}
		message.Source = message_info.Source
	}

	return result, nil
}

func (self *CryptoManager) Encrypt(
	plain_text []byte,
	destination string) (
	[]byte, error) {
	// The cipher is kept the same for all future communications
	// to enable the remote end to cache it - thereby saving RSA
	// operations for all messages in the session.
	var output_cipher *_Cipher

	output, ok := self.output_cipher_cache.Get(destination)
	if ok {
		output_cipher = output.(*_Cipher)
	} else {
		certificate, pres := self.GetCertificate(destination)
		if !pres {
			return nil, errors.New("No certificate found for destination")
		}

		cipher, err := _NewCipher(self.source, self.private_key, certificate)
		if err != nil {
			return nil, err
		}

		self.output_cipher_cache.Set(destination, cipher)
		output_cipher = cipher
	}

	var b bytes.Buffer
	w := zlib.NewWriter(&b)
	w.Write([]byte(plain_text))
	w.Close()

	packed_message_list := &crypto_proto.PackedMessageList{}
	packed_message_list.Compression = crypto_proto.PackedMessageList_ZCOMPRESSION.Enum()
	packed_message_list.MessageList = b.Bytes()
	packed_message_list.Source = &self.source

	serialized_packed_message_list, err := proto.Marshal(packed_message_list)
	if err != nil {
		return nil, err
	}

	var api_version uint32 = 3
	comms := &crypto_proto.ClientCommunication{
		EncryptedCipher:         output_cipher.encrypted_cipher,
		EncryptedCipherMetadata: output_cipher.encrypted_cipher_metadata,
		PacketIv:                make([]byte, output_cipher.key_size/8),
		ApiVersion:              &api_version,
	}

	// Each packet has a new IV.
	_, err = rand.Read(comms.PacketIv)
	if err != nil {
		return nil, err
	}

	encrypted_serialized_packed_message_list, err := encryptSymmetric(
		output_cipher.cipher_properties,
		serialized_packed_message_list,
		comms.PacketIv)
	if err != nil {
		return nil, err
	}

	comms.Encrypted = encrypted_serialized_packed_message_list
	comms.FullHmac = self.calcHMAC(comms, output_cipher.cipher_properties)

	result, err := proto.Marshal(comms)
	if err != nil {
		return nil, err
	}

	return result, nil
}