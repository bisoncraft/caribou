const hre = require("hardhat");

async function main() {
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
