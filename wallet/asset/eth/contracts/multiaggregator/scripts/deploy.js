const hre = require("hardhat");

const expectedChainId = { polygon: 137n, polygonAmoy: 80002n };

async function main() {
  const networkName = hre.network.name;
  const expected = expectedChainId[networkName];
  if (expected !== undefined) {
    const { chainId } = await hre.ethers.provider.getNetwork();
    if (chainId !== expected) {
      throw new Error(`Wrong chain: expected chainId ${expected} for ${networkName}, got ${chainId}`);
    }
  }

  const [deployer] = await hre.ethers.getSigners();
  const balance = await hre.ethers.provider.getBalance(deployer.address);
  if (balance === 0n) {
    throw new Error(`Deployer ${deployer.address} has zero balance on ${networkName}`);
  }

  const MultiAggregator = await hre.ethers.getContractFactory("MultiAggregatorV0");
  const contract = await MultiAggregator.deploy();
  await contract.waitForDeployment();
  const addr = await contract.getAddress();
  console.log("MultiAggregatorV0 deployed to:", addr);
  console.log("Network:", hre.network.name);
}

main()
  .then(() => process.exit(0))
  .catch((error) => {
    console.error(error);
    process.exit(1);
  });
