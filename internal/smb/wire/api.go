package wire

// M1 implements these typed functions. M0 records signatures only; it supplies
// no stub bodies. Every function returns an error for invalid encoding.
// Decoders copy returned byte slices. Offsets in command bodies are relative to
// that member's SMB header. Encoders return body bytes, not a header or TCP prefix.
// The server dispatches by Command before calling a command-specific decoder.
// Unknown context tags remain raw values for the handler to ignore or refuse.
//
// Framing functions validate the complete compound before returning any member.
// Join writes eight-byte padding and NextCommand links. EncodeHeader writes
// exactly 64 bytes and validates the selected synchronous or asynchronous layout.
// DecodeSMB1Negotiate accepts only an opening negotiate that offers SMB2.
//
//	func DecodeHeader(packet []byte) (Header, error)
//	func EncodeHeader(header Header) ([]byte, error)
//	func Split(packet []byte) ([]Message, error)
//	func Join(messages []Message) ([]byte, error)
//	func DecodeSMB1Negotiate(packet []byte) error
//
// Each request decoder checks its command, structure size and offsets. Each
// response decoder also checks the response flag. Error and pending responses
// use DecodeErrorResponse rather than a success-body decoder.
//
//	func DecodeNegotiateRequest(m Message) (NegotiateRequest, error)
//	func EncodeNegotiateRequest(r NegotiateRequest) ([]byte, error)
//	func DecodeNegotiateResponse(m Message) (NegotiateResponse, error)
//	func EncodeNegotiateResponse(r NegotiateResponse) ([]byte, error)
//	func DecodeSessionSetupRequest(m Message) (SessionSetupRequest, error)
//	func EncodeSessionSetupRequest(r SessionSetupRequest) ([]byte, error)
//	func DecodeSessionSetupResponse(m Message) (SessionSetupResponse, error)
//	func EncodeSessionSetupResponse(r SessionSetupResponse) ([]byte, error)
//	func DecodeTreeConnectRequest(m Message) (TreeConnectRequest, error)
//	func EncodeTreeConnectRequest(r TreeConnectRequest) ([]byte, error)
//	func DecodeTreeConnectResponse(m Message) (TreeConnectResponse, error)
//	func EncodeTreeConnectResponse(r TreeConnectResponse) ([]byte, error)
//	func DecodeCreateRequest(m Message) (CreateRequest, error)
//	func EncodeCreateRequest(r CreateRequest) ([]byte, error)
//	func DecodeCreateResponse(m Message) (CreateResponse, error)
//	func EncodeCreateResponse(r CreateResponse) ([]byte, error)
//	func DecodeCloseRequest(m Message) (CloseRequest, error)
//	func EncodeCloseRequest(r CloseRequest) ([]byte, error)
//	func DecodeCloseResponse(m Message) (CloseResponse, error)
//	func EncodeCloseResponse(r CloseResponse) ([]byte, error)
//	func DecodeFlushRequest(m Message) (FlushRequest, error)
//	func EncodeFlushRequest(r FlushRequest) ([]byte, error)
//	func DecodeFlushResponse(m Message) (EmptyResponse, error)
//	func EncodeFlushResponse(r EmptyResponse) ([]byte, error)
//	func DecodeReadRequest(m Message) (ReadRequest, error)
//	func EncodeReadRequest(r ReadRequest) ([]byte, error)
//	func DecodeReadResponse(m Message) (ReadResponse, error)
//	func EncodeReadResponse(r ReadResponse) ([]byte, error)
//	func DecodeWriteRequest(m Message) (WriteRequest, error)
//	func EncodeWriteRequest(r WriteRequest) ([]byte, error)
//	func DecodeWriteResponse(m Message) (WriteResponse, error)
//	func EncodeWriteResponse(r WriteResponse) ([]byte, error)
//	func DecodeLockRequest(m Message) (LockRequest, error)
//	func EncodeLockRequest(r LockRequest) ([]byte, error)
//	func DecodeLockResponse(m Message) (EmptyResponse, error)
//	func EncodeLockResponse(r EmptyResponse) ([]byte, error)
//	func DecodeQueryDirectoryRequest(m Message) (QueryDirectoryRequest, error)
//	func EncodeQueryDirectoryRequest(r QueryDirectoryRequest) ([]byte, error)
//	func DecodeQueryDirectoryResponse(m Message) (QueryResponse, error)
//	func EncodeQueryDirectoryResponse(r QueryResponse) ([]byte, error)
//	func DecodeQueryInfoRequest(m Message) (QueryInfoRequest, error)
//	func EncodeQueryInfoRequest(r QueryInfoRequest) ([]byte, error)
//	func DecodeQueryInfoResponse(m Message) (QueryResponse, error)
//	func EncodeQueryInfoResponse(r QueryResponse) ([]byte, error)
//	func DecodeSetInfoRequest(m Message) (SetInfoRequest, error)
//	func EncodeSetInfoRequest(r SetInfoRequest) ([]byte, error)
//	func DecodeSetInfoResponse(m Message) (EmptyResponse, error)
//	func EncodeSetInfoResponse(r EmptyResponse) ([]byte, error)
//	func DecodeIOCTLRequest(m Message) (IOCTLRequest, error)
//	func EncodeIOCTLRequest(r IOCTLRequest) ([]byte, error)
//	func DecodeIOCTLResponse(m Message) (IOCTLResponse, error)
//	func EncodeIOCTLResponse(r IOCTLResponse) ([]byte, error)
//	func DecodeLeaseBreakRequest(m Message) (LeaseBreakRequest, error)
//	func EncodeLeaseBreakRequest(r LeaseBreakRequest) ([]byte, error)
//	func DecodeLeaseBreakResponse(m Message) (LeaseBreakResponse, error)
//	func EncodeLeaseBreakResponse(r LeaseBreakResponse) ([]byte, error)
//	func DecodeLeaseBreakNotification(m Message) (LeaseBreakNotification, error)
//	func EncodeLeaseBreakNotification(r LeaseBreakNotification) ([]byte, error)
//	func DecodeChangeNotifyRequest(m Message) (ChangeNotifyRequest, error)
//	func EncodeChangeNotifyRequest(r ChangeNotifyRequest) ([]byte, error)
//	func DecodeErrorResponse(m Message) (ErrorResponse, error)
//	func EncodeErrorResponse(r ErrorResponse) ([]byte, error)
//
// Empty-body commands still have separate typed functions and command-specific
// structure sizes. CANCEL has no response encoder because it has no reply.
//
//	func DecodeEchoRequest(m Message) (EmptyRequest, error)
//	func EncodeEchoRequest(r EmptyRequest) ([]byte, error)
//	func DecodeEchoResponse(m Message) (EmptyResponse, error)
//	func EncodeEchoResponse(r EmptyResponse) ([]byte, error)
//	func DecodeLogoffRequest(m Message) (EmptyRequest, error)
//	func EncodeLogoffRequest(r EmptyRequest) ([]byte, error)
//	func DecodeLogoffResponse(m Message) (EmptyResponse, error)
//	func EncodeLogoffResponse(r EmptyResponse) ([]byte, error)
//	func DecodeTreeDisconnectRequest(m Message) (EmptyRequest, error)
//	func EncodeTreeDisconnectRequest(r EmptyRequest) ([]byte, error)
//	func DecodeTreeDisconnectResponse(m Message) (EmptyResponse, error)
//	func EncodeTreeDisconnectResponse(r EmptyResponse) ([]byte, error)
//	func DecodeCancelRequest(m Message) (EmptyRequest, error)
//	func EncodeCancelRequest(r EmptyRequest) ([]byte, error)
//
// Context functions check the tag or type and the data length. Encoders return a
// tagged context, including its data but excluding the chain header. The command
// codec encodes the chain. Separate query and reply decoders avoid ambiguous
// equal-length layouts. Reserved lease acknowledgment bytes are ignored.
//
//	func DecodePreauthContext(c NegotiateContext) (PreauthContext, error)
//	func EncodePreauthContext(c PreauthContext) (NegotiateContext, error)
//	func DecodeEncryptionContext(c NegotiateContext) (EncryptionContext, error)
//	func EncodeEncryptionContext(c EncryptionContext) (NegotiateContext, error)
//	func DecodeSigningContext(c NegotiateContext) (SigningContext, error)
//	func EncodeSigningContext(c SigningContext) (NegotiateContext, error)
//	func DecodeAAPLQuery(c CreateContext) (AAPLQuery, error)
//	func EncodeAAPLQuery(c AAPLQuery) (CreateContext, error)
//	func DecodeAAPLReply(c CreateContext) (AAPLReply, error)
//	func EncodeAAPLReply(c AAPLReply) (CreateContext, error)
//	func DecodeMaxAccessQuery(c CreateContext) (MaxAccessQuery, error)
//	func EncodeMaxAccessQuery(c MaxAccessQuery) (CreateContext, error)
//	func DecodeMaxAccessReply(c CreateContext) (MaxAccessReply, error)
//	func EncodeMaxAccessReply(c MaxAccessReply) (CreateContext, error)
//	func DecodeFileIDQuery(c CreateContext) (FileIDQuery, error)
//	func EncodeFileIDQuery(c FileIDQuery) (CreateContext, error)
//	func DecodeFileIDReply(c CreateContext) (FileIDReply, error)
//	func EncodeFileIDReply(c FileIDReply) (CreateContext, error)
//	func DecodeDurableRequest(c CreateContext) (DurableRequest, error)
//	func EncodeDurableRequest(c DurableRequest) (CreateContext, error)
//	func DecodeDurableReply(c CreateContext) (DurableReply, error)
//	func EncodeDurableReply(c DurableReply) (CreateContext, error)
//	func DecodeDurableReconnect(c CreateContext) (DurableReconnect, error)
//	func EncodeDurableReconnect(c DurableReconnect) (CreateContext, error)
//	func DecodeLeaseContext(c CreateContext) (LeaseContext, error)
//	func EncodeLeaseContext(c LeaseContext) (CreateContext, error)
