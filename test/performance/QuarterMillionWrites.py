from testmodule import *
import time

class TestWrites(TestRunner):

	def __init__(self):
		super().__init__()

	def mthd(self):

		import sharkbite

		tableOperations = super().getTableOperations()

		if not tableOperations.exists(False):
		    print ("Creating table")
		    if not tableOperations.create(False):
		    	print("Could not create table")
		else:
			tableOperations.remove()
			time.sleep(2)
			tableOperations.create(False)
			print ("Table already exists, so not creating it")

		tableOperations = super().newtableOperations()
		splits = {"row","row5","row9", "sow"}
		tableOperations.addSplits(splits)

		auths = sharkbite.Authorizations()

		""" Add authorizations """
		""" mutation.put("cf","cq","cv",1569786960) """

		writer = tableOperations.createWriter(auths, 10)

		rng = range(0,500)
		for i in rng:
			row = ("row%i" % (i+5))
			mutation = sharkbite.Mutation(row);
			rng2 = range(0,100)
			for j in rng:
				mutation.put("cf%i" % j ,"cq","",1569786960, "value")
			writer.addMutation(mutation)
			print("wrote %i entries" % ((i+1)*(j+1)))

		writer.close()

		tableOperations.compact("a","s",True)

		print("written")

		scanner = tableOperations.createScanner(auths, 2)

		time.sleep(1)

		accumuloRange = sharkbite.Range("a",True,"sow",False)

		scanner.addRange( accumuloRange )

		resultset = scanner.getResultSet()
		count=0
		for keyvalue in resultset:
			key = keyvalue.getKey()
			count=count+1
		print("count is ", count)

		""" delete your table if user did not create temp """

#		tableOperations.remove()


runner = TestWrites()
runner.mthd()
