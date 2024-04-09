export GAIAD_NODE="tcp://localhost:26657"

FLAGS="--gas=2500000 --from=validator --keyring-backend=test --chain-id=local-1 --output=json --yes"

gaiad tx wasm store ./contrib/cw_template.wasm $FLAGS

txhash=$(gaiad tx wasm instantiate 1 '{"count":0}' --label=cw_template --no-admin $FLAGS | jq -r .txhash) && echo $txhash

addr=$(gaiad q tx $txhash --output=json | jq -r .logs[0].events[2].attributes[0].value) && echo $addr

gaiad q wasm contract $addr