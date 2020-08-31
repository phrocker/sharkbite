from testmodule import *
import sys
import time

class TestWrites(TestRunner):
		
	def __init__(self):
		super().__init__()
	
	def mthd(self):
	
		import pysharkbite

		
		tableOperations = super().getTableOperations()

		## validate that table is removed
		try:			
			if not tableOperations.exists(False):
			    print ("Creating table")
			    if not tableOperations.create(False):
			    	print("Could not create table")  
			else:
			    print ("Table already exists, so not creating it")  
			
			
			auths = pysharkbite.Authorizations()
			
			""" Add authorizations """ 
			""" mutation.put("cf","cq","cv",1569786960) """
			
			tableOperations.remove()
			
			time.sleep(1)
			
			writer = tableOperations.createWriter(auths, 10)
			
			mutation = pysharkbite.Mutation("sow2");    
			
			mutation.put("cf","cq","",1569786960, "value")
			mutation.put("cf2","cq2","",1569786960, "value2")
			""" no value """
			mutation.put("cf3","cq3","",1569786960, "") 
			
			writer.addMutation( mutation )
			
			writer.close()
			
			writer = tableOperations.createWriter(auths, 10)
			
			rng = range(0,1000)
			for i in rng:
				row = ("row%i" % (i+5))
				mutation = pysharkbite.Mutation(row);    
				mutation.put("cf","cq","",1569786960, "value")
				writer.addMutation(mutation)
				
			writer.close()
			
			print("Table not removed")
			sys.exit(1)
		except (pysharkbite.ClientException, RuntimeError, TypeError, NameError):
			print("Table successfully removed")
			# this is okay
			pass
		try:
			testzk = pysharkbite.ZookeeperInstance(None, None, 1000, None)
			print("Table not removed")
			sys.exit(1)
		except (RuntimeError, TypeError, NameError):
			print("Caught expected error")
			# this is okay
			
		try:
			writer = tableOperations.createWriter(None, 10)
			print("Expected error passing None")
			sys.exit(1)
		except (pysharkbite.ClientException,RuntimeError, TypeError, NameError):
			print("Caught expected error")
			# this is okay
		
		tableOperations.remove()


runner = TestWrites()
runner.mthd()